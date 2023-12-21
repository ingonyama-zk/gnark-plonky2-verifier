// This package implements efficient Golidlocks arithmetic operations within Gnark. We do not use
// the emulated field arithmetic API, because it is too slow for our purposes. Instead, we use
// an efficient reduction method that leverages the fact that the modulus is a simple
// linear combination of powers of two.
package goldilocks

// In general, methods whose name do not contain `NoReduce` can be used without any extra mental
// overhead. These methods act exactly as you would expect a normal field would operate.
//
// However, if you want to aggressively optimize the number of constraints in your circuit, it can
// be very beneficial to use the no reduction methods and keep track of the maximum number of bits
// your computation uses.

// This implementation is based on the following plonky2 implementation of Goldilocks
// Available here: https://github.com/0xPolygonZero/plonky2/blob/main/field/src/goldilocks_field.rs#L70

import (
	"fmt"
	"math"
	"math/big"

	"github.com/consensys/gnark-crypto/field/goldilocks"
	"github.com/consensys/gnark/constraint/solver"
	"github.com/consensys/gnark/frontend"
	"github.com/consensys/gnark/std/math/emulated"
	"github.com/consensys/gnark/std/rangecheck"
)

// The multiplicative group generator of the field.
var MULTIPLICATIVE_GROUP_GENERATOR goldilocks.Element = goldilocks.NewElement(7)

// The two adicity of the field.
var TWO_ADICITY uint64 = 32

// The power of two generator of the field.
var POWER_OF_TWO_GENERATOR goldilocks.Element = goldilocks.NewElement(1753635133440165772)

// The modulus of the field.
var MODULUS *big.Int = emulated.Goldilocks{}.Modulus()

// The number of bits to use for range checks on inner products of field elements.
var RANGE_CHECK_NB_BITS int = 140

// Registers the hint functions with the solver.
func init() {
	solver.RegisterHint(MulAddHint)
	solver.RegisterHint(ReduceHint)
	solver.RegisterHint(InverseHint)
	solver.RegisterHint(SplitLimbsHint)
}

// A type alias used to represent Goldilocks field elements.
type Variable struct {
	Limb frontend.Variable
}

// Creates a new Goldilocks field element from an existing variable. Assumes that the element is
// already reduced.
func NewVariable(x frontend.Variable) Variable {
	return Variable{Limb: x}
}

// The zero element in the Golidlocks field.
func Zero() Variable {
	return NewVariable(0)
}

// The one element in the Goldilocks field.
func One() Variable {
	return NewVariable(1)
}

// The negative one element in the Goldilocks field.
func NegOne() Variable {
	return NewVariable(MODULUS.Uint64() - 1)
}

// The chip used for Goldilocks field operations.
type Chip struct {
	api          frontend.API
	rangeChecker frontend.Rangechecker
}

// Creates a new Goldilocks Chip.
func New(api frontend.API) *Chip {
	rangeChecker := rangecheck.New(api)
	return &Chip{api: api, rangeChecker: rangeChecker}
}

// Adds two goldilocks field elements and returns a value within the goldilocks field.
func (p *Chip) Add(a Variable, b Variable) Variable {
	return p.MulAdd(a, NewVariable(1), b)
}

// Adds two goldilocks field elements and returns a value that may not be within the goldilocks field
// (e.g. the sum is not reduced).
func (p *Chip) AddNoReduce(a Variable, b Variable) Variable {
	return NewVariable(p.api.Add(a.Limb, b.Limb))
}

// Subracts two goldilocks field elements and returns a value within the goldilocks field.
func (p *Chip) Sub(a Variable, b Variable) Variable {
	return p.MulAdd(b, NegOne(), a)
}

// Subracts two goldilocks field elements and returns a value that may not be within the goldilocks field
// (e.g. the difference is not reduced).
func (p *Chip) SubNoReduce(a Variable, b Variable) Variable {
	return NewVariable(p.api.Add(a.Limb, p.api.Mul(b.Limb, NegOne().Limb)))
}

// Multiplies two goldilocks field elements and returns a value within the goldilocks field.
func (p *Chip) Mul(a Variable, b Variable) Variable {
	return p.MulAdd(a, b, Zero())
}

// Multiplies two goldilocks field elements and returns a value that may not be within the goldilocks field
// (e.g. the product is not reduced).
func (p *Chip) MulNoReduce(a Variable, b Variable) Variable {
	return NewVariable(p.api.Mul(a.Limb, b.Limb))
}

// Multiplies two field elements and adds a field element (e.g. computes a * b + c).  The returned value
// will be within the goldilocks field.
func (p *Chip) MulAdd(a Variable, b Variable, c Variable) Variable {
	result, err := p.api.Compiler().NewHint(MulAddHint, 2, a.Limb, b.Limb, c.Limb)
	if err != nil {
		panic(err)
	}

	quotient := NewVariable(result[0])
	remainder := NewVariable(result[1])

	cLimbCopy := p.api.Mul(c.Limb, 1)
	lhs := p.api.MulAcc(cLimbCopy, a.Limb, b.Limb)
	rhs := p.api.MulAcc(remainder.Limb, MODULUS, quotient.Limb)
	p.api.AssertIsEqual(lhs, rhs)

	p.RangeCheck(quotient)
	p.RangeCheck(remainder)
	return remainder
}

// Multiplies two field elements and adds a field element (e.g. computes a * b + c).  The returned value
// may no be within the goldilocks field (e.g. the result is not reduced).
func (p *Chip) MulAddNoReduce(a Variable, b Variable, c Variable) Variable {
	cLimbCopy := p.api.Mul(c.Limb, 1)
	return NewVariable(p.api.MulAcc(cLimbCopy, a.Limb, b.Limb))
}

// The hint used to compute MulAdd.
func MulAddHint(_ *big.Int, inputs []*big.Int, results []*big.Int) error {
	if len(inputs) != 3 {
		panic("MulAddHint expects 3 input operands")
	}

	for _, operand := range inputs {
		if operand.Cmp(MODULUS) >= 0 {
			panic(fmt.Sprintf("%s is not in the field", operand.String()))
		}
	}

	product := new(big.Int).Mul(inputs[0], inputs[1])
	sum := new(big.Int).Add(product, inputs[2])
	quotient := new(big.Int).Div(sum, MODULUS)
	remainder := new(big.Int).Rem(sum, MODULUS)

	results[0] = quotient
	results[1] = remainder

	return nil
}

// Reduces a field element x such that x % MODULUS = y.
func (p *Chip) Reduce(x Variable) Variable {
	// Witness a `quotient` and `remainder` such that:
	//
	// 		MODULUS * quotient + remainder = x
	//
	// Must check that offset \in [0, MODULUS) and carry \in [0, 2^RANGE_CHECK_NB_BITS) to ensure
	// that this computation does not overflow. We use 2^RANGE_CHECK_NB_BITS to reduce the cost of the range check
	//
	// In other words, we assume that we at most compute a a dot product with dimension at most RANGE_CHECK_NB_BITS - 128.
	return p.ReduceWithMaxBits(x, uint64(RANGE_CHECK_NB_BITS))
}

// Reduces a field element x such that x % MODULUS = y.
func (p *Chip) ReduceWithMaxBits(x Variable, maxNbBits uint64) Variable {
	// Witness a `quotient` and `remainder` such that:
	//
	// 		MODULUS * quotient + remainder = x
	//
	// Must check that remainder \in [0, MODULUS) and quotient \in [0, 2^maxNbBits) to ensure that this
	// computation does not overflow.

	result, err := p.api.Compiler().NewHint(ReduceHint, 2, x.Limb)
	if err != nil {
		panic(err)
	}

	quotient := result[0]
	p.rangeChecker.Check(quotient, int(maxNbBits))

	remainder := NewVariable(result[1])
	p.RangeCheck(remainder)

	p.api.AssertIsEqual(x.Limb, p.api.Add(p.api.Mul(quotient, MODULUS), remainder.Limb))

	return remainder
}

// The hint used to compute Reduce.
func ReduceHint(_ *big.Int, inputs []*big.Int, results []*big.Int) error {
	if len(inputs) != 1 {
		panic("ReduceHint expects 1 input operand")
	}
	input := inputs[0]
	quotient := new(big.Int).Div(input, MODULUS)
	remainder := new(big.Int).Rem(input, MODULUS)
	results[0] = quotient
	results[1] = remainder
	return nil
}

// Computes the inverse of a field element x such that x * x^-1 = 1.
func (p *Chip) Inverse(x Variable) (Variable, frontend.Variable) {
	result, err := p.api.Compiler().NewHint(InverseHint, 2, x.Limb)
	if err != nil {
		panic(err)
	}

	inverse := NewVariable(result[0])
	hasInv := frontend.Variable(result[1])
	p.RangeCheck(inverse)

	product := p.Mul(inverse, x)
	productToCheck := p.api.Select(hasInv, product.Limb, frontend.Variable(1))
	p.api.AssertIsEqual(productToCheck, frontend.Variable(1))

	return inverse, hasInv
}

// The hint used to compute Inverse.
func InverseHint(_ *big.Int, inputs []*big.Int, results []*big.Int) error {
	if len(inputs) != 1 {
		panic("InverseHint expects 1 input operand")
	}

	input := inputs[0]
	if input.Cmp(MODULUS) == 0 || input.Cmp(MODULUS) == 1 {
		panic("Input is not in the field")
	}

	inputGl := goldilocks.NewElement(input.Uint64())
	resultGl := goldilocks.NewElement(0)

	// Will set resultGL if inputGL == 0
	resultGl.Inverse(&inputGl)

	result := big.NewInt(0)
	results[0] = resultGl.BigInt(result)

	hasInvInt64 := int64(0)
	if !inputGl.IsZero() {
		hasInvInt64 = 1
	}
	results[1] = big.NewInt(hasInvInt64)

	return nil
}

// The hint used to split a GoldilocksVariable into 2 32 bit limbs.
func SplitLimbsHint(_ *big.Int, inputs []*big.Int, results []*big.Int) error {
	if len(inputs) != 1 {
		panic("SplitLimbsHint expects 1 input operand")
	}

	// The Goldilocks field element
	input := inputs[0]

	if input.Cmp(MODULUS) == 0 || input.Cmp(MODULUS) == 1 {
		return fmt.Errorf("input is not in the field")
	}

	two_32 := big.NewInt(int64(math.Pow(2, 32)))

	// The most significant bits
	results[0] = new(big.Int).Quo(input, two_32)
	// The least significant bits
	results[1] = new(big.Int).Rem(input, two_32)

	return nil
}

// Range checks a field element x to be less than the Golidlocks modulus 2 ^ 64 - 2 ^ 32 + 1.
func (p *Chip) RangeCheck(x Variable) {
	// The Goldilocks' modulus is 2^64 - 2^32 + 1, which is:
	//
	// 		1111111111111111111111111111111100000000000000000000000000000001
	//
	// in big endian binary. This function will first verify that x is at most 64 bits wide. Then it
	// checks that if the bits[0:31] (in big-endian) are all 1, then bits[32:64] are all zero.

	result, err := p.api.Compiler().NewHint(SplitLimbsHint, 2, x.Limb)
	if err != nil {
		panic(err)
	}

	// We check that this is a valid decomposition of the Goldilock's element and range-check each limb.
	mostSigLimb := result[0]
	leastSigLimb := result[1]
	p.api.AssertIsEqual(
		p.api.Add(
			p.api.Mul(mostSigLimb, uint64(math.Pow(2, 32))),
			leastSigLimb,
		),
		x.Limb,
	)
	p.rangeChecker.Check(mostSigLimb, 32)
	p.rangeChecker.Check(leastSigLimb, 32)

	// If the most significant bits are all 1, then we need to check that the least significant bits are all zero
	// in order for element to be less than the Goldilock's modulus.
	// Otherwise, we don't need to do any checks, since we already know that the element is less than the Goldilocks modulus.
	shouldCheck := p.api.IsZero(p.api.Sub(mostSigLimb, uint64(math.Pow(2, 32))-1))
	p.api.AssertIsEqual(
		p.api.Select(
			shouldCheck,
			leastSigLimb,
			frontend.Variable(0),
		),
		frontend.Variable(0),
	)
}

func (p *Chip) AssertIsEqual(x, y Variable) {
	p.api.AssertIsEqual(x.Limb, y.Limb)
}

// Computes the n'th primitive root of unity for the Goldilocks field.
func PrimitiveRootOfUnity(nLog uint64) goldilocks.Element {
	if nLog > TWO_ADICITY {
		panic("nLog is greater than TWO_ADICITY")
	}
	res := goldilocks.NewElement(POWER_OF_TWO_GENERATOR.Uint64())
	for i := 0; i < int(TWO_ADICITY-nLog); i++ {
		res.Square(&res)
	}
	return res
}

func TwoAdicSubgroup(nLog uint64) []goldilocks.Element {
	if nLog > TWO_ADICITY {
		panic("nLog is greater than GOLDILOCKS_TWO_ADICITY")
	}

	var res []goldilocks.Element
	rootOfUnity := PrimitiveRootOfUnity(nLog)
	res = append(res, goldilocks.NewElement(1))

	for i := 0; i < (1<<nLog)-1; i++ {
		lastElement := res[len(res)-1]
		res = append(res, *lastElement.Mul(&lastElement, &rootOfUnity))
	}

	return res
}
