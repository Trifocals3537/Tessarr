// Package par2 reads bounded PAR2 recovery sets and reconstructs missing slices.
package par2

import (
	"context"
	"encoding/binary"
	"errors"
	"sync"
)

// PAR2 uses GF(2^16), primitive polynomial x^16+x^12+x^3+x+1,
// little-endian words, and generators whose order is 65535.
// References: https://parchive.sourceforge.net/docs/specifications/parity-volume-spec/article-spec.html
// and https://github.com/akalin/gopar/tree/main/par2.
// Tables have fixed size; there is no coefficient-dependent allocation/cache.
var fieldOnce sync.Once
var logarithms [65536]uint16
var powers [131070]uint16
var generators [32768]uint16

func initField() {
	fieldOnce.Do(func() {
		x := uint32(1)
		for i := 0; i < 65535; i++ {
			powers[i] = uint16(x)
			logarithms[x] = uint16(i)
			x <<= 1
			if x&65536 != 0 {
				x ^= 0x1100b
			}
		}
		copy(powers[65535:], powers[:65535])
		n := 0
		for i := 1; i < 65535; i++ {
			if i%3 != 0 && i%5 != 0 && i%17 != 0 && i%257 != 0 {
				generators[n] = powers[i]
				n++
			}
		}
	})
}

func multiply(a, b uint16) uint16 {
	if a == 0 || b == 0 {
		return 0
	}
	return powers[int(logarithms[a])+int(logarithms[b])]
}

func coefficient(index int, exponent uint32) uint16 {
	return powers[(uint64(logarithms[generators[index]])*uint64(exponent))%65535]
}

func addScaled(ctx context.Context, dst, src []byte, c uint16, yield func(context.Context) error) error {
	if c == 0 {
		return nil
	}
	for start := 0; start < len(dst); start += 64 << 10 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if yield != nil {
			if err := yield(ctx); err != nil {
				return err
			}
		}
		end := min(start+64<<10, len(dst))
		for i := start; i < end; i += 2 {
			x := multiply(binary.LittleEndian.Uint16(src[i:]), c)
			binary.LittleEndian.PutUint16(dst[i:], binary.LittleEndian.Uint16(dst[i:])^x)
		}
	}
	return nil
}

// Solver holds one accumulator per verified recovery row. A missing slice can
// be discovered during the single input sweep; it must never have been folded.
type Solver struct {
	rows      [][]byte
	exponents []uint32
	missing   []int
	seen      map[int]bool
	size      int
	yield     func(context.Context) error
}

func NewSolver(size int, exponents []uint32, rows [][]byte, yield func(context.Context) error) (*Solver, error) {
	if size <= 0 || size > MaxSliceBytes || size%4 != 0 || len(rows) == 0 || len(rows) > MaxRecoveryRows || len(rows) != len(exponents) {
		return nil, errors.New("invalid recovery solver dimensions")
	}
	seen := make(map[uint32]bool)
	for i, row := range rows {
		if len(row) != size || seen[exponents[i]] {
			return nil, errors.New("invalid or duplicate recovery row")
		}
		seen[exponents[i]] = true
	}
	initField()
	return &Solver{rows: rows, exponents: exponents, seen: make(map[int]bool), size: size, yield: yield}, nil
}

func (s *Solver) Missing(index int) error {
	if index < 0 || index >= len(generators) || s.seen[index] {
		return errors.New("invalid or duplicate input slice")
	}
	if len(s.missing) >= len(s.rows) {
		return errors.New("insufficient usable recovery slices")
	}
	s.seen[index] = true
	s.missing = append(s.missing, index)
	return nil
}

func (s *Solver) Fold(ctx context.Context, index int, data []byte) error {
	if index < 0 || index >= len(generators) || s.seen[index] || len(data) != s.size {
		return errors.New("invalid or duplicate input slice")
	}
	s.seen[index] = true
	for i, row := range s.rows {
		if err := addScaled(ctx, row, data, coefficient(index, s.exponents[i]), s.yield); err != nil {
			return err
		}
	}
	return nil
}

// Solve reduces all rows, so dependent Vandermonde rows are skipped rather
// than assuming that the first k recovery exponents form an invertible matrix.
// Returned buffers alias the solver's accumulators and must remain immutable.
func (s *Solver) Solve(ctx context.Context) (map[int][]byte, error) {
	k := len(s.missing)
	result := make(map[int][]byte, k)
	if k == 0 {
		return result, nil
	}
	a := make([][]uint16, len(s.rows))
	for r := range a {
		a[r] = make([]uint16, k)
		for c, index := range s.missing {
			a[r][c] = coefficient(index, s.exponents[r])
		}
	}
	for c := 0; c < k; c++ {
		pivot := c
		for pivot < len(a) && a[pivot][c] == 0 {
			pivot++
		}
		if pivot == len(a) {
			return nil, errors.New("recovery matrix has insufficient independent rows")
		}
		a[c], a[pivot] = a[pivot], a[c]
		s.rows[c], s.rows[pivot] = s.rows[pivot], s.rows[c]
		inverse := powers[65535-int(logarithms[a[c][c]])]
		for j := c; j < k; j++ {
			a[c][j] = multiply(a[c][j], inverse)
		}
		// Multiplication by the inverse in place, without an extra slice buffer.
		for off := 0; off < s.size; off += 64 << 10 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if s.yield != nil {
				if err := s.yield(ctx); err != nil {
					return nil, err
				}
			}
			for p := off; p < min(off+64<<10, s.size); p += 2 {
				binary.LittleEndian.PutUint16(s.rows[c][p:], multiply(binary.LittleEndian.Uint16(s.rows[c][p:]), inverse))
			}
		}
		for r := range a {
			if r == c || a[r][c] == 0 {
				continue
			}
			factor := a[r][c]
			for j := c; j < k; j++ {
				a[r][j] ^= multiply(a[c][j], factor)
			}
			if err := addScaled(ctx, s.rows[r], s.rows[c], factor, s.yield); err != nil {
				return nil, err
			}
		}
	}
	for i, index := range s.missing {
		result[index] = s.rows[i]
	}
	return result, nil
}
