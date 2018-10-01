package random

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
)

var errInvalid = errors.New("invalid input value")

// Distribution is used to represent a distribution value.
// The underlying implementation depends on the distribution's type.
type Distribution interface {
	Generate() int64
}

var (
	_ Distribution  = (*Fixed)(nil)
	_ Distribution  = (*Uniform)(nil)
	_ Distribution  = (*Ratio)(nil)
	_ rand.Source64 = (*lockedSource)(nil)
)

// globalRand is a copy of the rand.globalRand (git.io/fA2Ls) for
// usermode package's use only.
var globalRand = rand.New(&lockedSource{
	src: rand.NewSource(
		time.Now().UnixNano() + int64(os.Getpid()),
	).(rand.Source64),
})

// Product gives a product of values generated by the given distributions.
// The smallest value returned by the function is 1.
func Product(d ...Distribution) int64 {
	n, ratio := int64(1), int64(1)
	for _, d := range d {
		n *= d.Generate()
		if r, ok := d.(*Ratio); ok {
			ratio *= r.Value
		}
	}
	return max(n/ratio, 1)
}

// Generator is used to generate data for a column basing on random seeds.
// Generator keep tracks of the seeds to ensure proper population of
// generated values.
type Generator struct {
	mu    sync.Mutex // protects seeds
	seeds map[seed]struct{}
}

type seed struct {
	column string
	value  int64
}

// NewGenerator gives new generator value.
func NewGenerator() *Generator {
	return &Generator{
		seeds: make(map[seed]struct{}),
	}
}

// Generate fills the out argument with random data. The argument must be a pointer.
// Currently supported types are int and string.
//
// The population argument controls the uniqueness distribution of the generated
// data. The size argument controls the size in bytes of the generated data.
func (g *Generator) Generate(population, size Distribution, out interface{}) {
	g.generate(population.Generate(), size, out)
}

// GenerateUnique fills the out argument with random data. The argument must
// be a pointer. Currently supported types are int and string.
//
// The function returns true if a unique seed was generated for the given column
// name, false otherwise.
//
// The population argument controls the uniqueness distribution of the generated
// data. The size argument controls the size in bytes of the generated data.
func (g *Generator) GenerateUnique(name string, population, size Distribution, out interface{}) bool {
	seed, ok := g.generateSeed(name, population)
	if !ok {
		return false
	}
	g.generate(seed, size, out)
	return true
}

func (g *Generator) generate(seed int64, size Distribution, out interface{}) {
	const pad = "x"
	switch out := out.(type) {
	case *int:
		*out = int(seed)
	case *string:
		p := make([]byte, 8)
		binary.LittleEndian.PutUint64(p, uint64(seed))
		*out = hex.EncodeToString(p)
		if n := int(size.Generate()) - len(*out); n > 0 {
			*out += strings.Repeat(pad, n)
		}
	default:
		panic(fmt.Errorf("unsupported type: %T", out))
	}
}

func (g *Generator) generateSeed(column string, d Distribution) (int64, bool) {
	s := seed{column, d.Generate()}
	g.mu.Lock()
	_, ok := g.seeds[s]
	if !ok {
		g.seeds[s] = struct{}{}
	}
	g.mu.Unlock()
	return s.value, !ok
}

// ParseDistribution parses a distribution string. See "Supported types" section
// of a document located under the following url for more details:
//
//   https://cassandra.apache.org/doc/latest/tools/cassandra_stress.html#profile
//
// TODO(rjeczalik): Add support for inverted distributions.
// TODO(rjeczalik): Add support for short syntax declaration (e.g. "uniform(1..1K)").
// TODO(rjeczalik): Add support for more distribution types.
func ParseDistribution(s string) (Distribution, error) {
	i := strings.IndexRune(s, '(')
	if i == -1 || i == 0 {
		return nil, errors.New("missing parameter list start delimiter '('")
	}
	j := strings.IndexRune(s, ')')
	if j == -1 || i > j {
		return nil, errors.New("missing parameter list end delimiter ')'")
	}
	typ, val := s[:i], s[i+1:j]
	if typ[0] == '~' {
		return nil, errors.New("unsupported inverted distribution: " + typ)
	}
	switch typ {
	case "fixed":
		n, err := strconv.ParseUint(val, 10, 32)
		if err != nil {
			return nil, errors.Wrap(err, "value for fixed distribution is invalid")
		}
		return &Fixed{
			Value: int64(n),
		}, nil
	case "uniform":
		p := strings.Split(s[i+1:j], "..")
		if len(p) != 2 {
			return nil, errors.New("interval for uniform distribution has invalid format, expected: min..max")
		}
		min, err := strconv.ParseUint(p[0], 10, 32)
		if err != nil {
			return nil, errors.Wrap(err, "min parameter uniform distribution is invalid")
		}
		max, err := strconv.ParseUint(p[1], 10, 32)
		if err != nil {
			return nil, errors.Wrap(err, "max parameter for uniform distribution is invalid")
		}
		if max < min {
			return nil, errors.New("interval for uniform distribution is invalid: min >= max")
		}
		return &Uniform{
			Min: int64(min),
			Max: int64(max),
		}, nil
	default:
		return nil, errors.New("unsupported distribution: " + typ)
	}
}

// Ratio describes how likely certain operation is going to happen.
//
// For example the ratio represented by "fixed(1)/1" string says
// that an operation is going to apply for the whole partition, whilst
// the "fixed(1)/2" one - only for the half.
//
// Ratio is used for describing size of a batch insert depending on
// parition size (the "select" statement).
type Ratio struct {
	Distribution
	Value int64
}

// ParseRatio parses a ratio string.
//
// For example the "uniform(1..10)/10" ratio is parsed to the
// following value:
//
//   &Ratio{
//     Distribution: &Uniform{Min: 1, Max: 10},
//     Value: 10,
//   }
//
func ParseRatio(s string) (*Ratio, error) {
	d, err := ParseDistribution(s)
	if err != nil {
		return nil, err
	}
	i := strings.IndexRune(s, '/')
	if i == -1 {
		return nil, errInvalid
	}
	n, err := strconv.ParseUint(s[i+1:], 10, 32)
	if err != nil {
		return nil, errInvalid
	}
	if n == 0 {
		return nil, errInvalid
	}
	return &Ratio{
		Distribution: d,
		Value:        int64(n),
	}, nil
}

// String implements the fmt.Stringer interface.
func (r *Ratio) String() string {
	return fmt.Sprintf("%s/%d", r.Distribution, r.Value)
}

// Generate implements the Distribution interface.
func (r *Ratio) Generate() int64 {
	return r.Distribution.Generate()
}

// Fixed represents a fixed distribution, that always returns specified value.
type Fixed struct {
	Value int64
}

// String implements the fmt.Stringer interface.
func (f Fixed) String() string {
	return fmt.Sprintf("Fixed(%d)", f.Value)
}

// Generate implements the Distribution interface.
func (f Fixed) Generate() int64 {
	return f.Value
}

// Uniform represents a uniform distribution over specified [Min, Max] range.
type Uniform struct {
	Min, Max int64 // upper and lower bound of the distribution
}

// String implements the fmt.Stringer interface.
func (u Uniform) String() string {
	return fmt.Sprintf("Uniform(min=%d, max=%d)", u.Min, u.Max)
}

// Generate implements the Distribution interface.
func (u Uniform) Generate() int64 {
	return u.Min + globalRand.Int63n(u.Max-u.Min)
}

func max(i, j int64) int64 {
	if i > j {
		return i
	}
	return j
}

type lockedSource struct {
	mu  sync.Mutex
	src rand.Source64
}

func (r *lockedSource) Int63() (n int64) {
	r.mu.Lock()
	n = r.src.Int63()
	r.mu.Unlock()
	return
}

func (r *lockedSource) Uint64() (n uint64) {
	r.mu.Lock()
	n = r.src.Uint64()
	r.mu.Unlock()
	return
}

func (r *lockedSource) Seed(seed int64) {
	r.mu.Lock()
	r.src.Seed(seed)
	r.mu.Unlock()
}
