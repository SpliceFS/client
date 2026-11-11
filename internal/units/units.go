// Package units defines the size and duration types used in the YAML
// configuration files of both the server and the client.
//
// Sizes take the binary suffixes KiB, MiB, GiB and TiB. Durations take one of
// the suffixes s, m, h and d (24 hours). Both accept the word "unlimited".
package units

import (
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// quantityPattern splits a value into integer digits, fraction digits and a
// unit. The unit is whatever follows the number and is checked separately.
var quantityPattern = regexp.MustCompile(`^([0-9]+)(?:\.([0-9]+))?([^0-9.]*)$`)

const unlimitedWord = "unlimited"

// Errors reported by parseScaled. They are wrapped with the offending value by
// the callers.
var (
	errNoNumber = fmt.Errorf("not a number followed by a unit")
	errNoUnit   = fmt.Errorf("missing unit")
	errNotWhole = fmt.Errorf("does not come to a whole number of base units")
	errTooLarge = fmt.Errorf("too large")
)

// parseScaled parses a non-negative integer or decimal number followed by a
// unit. multiplier maps a unit to its size in base units. The result must be a
// whole number of base units and must be smaller than math.MaxInt64, which is
// reserved for "unlimited".
func parseScaled(text string, multiplier func(unit string) (int64, bool)) (int64, error) {
	m := quantityPattern.FindStringSubmatch(text)
	if m == nil {
		return 0, errNoNumber
	}
	mult, ok := multiplier(m[3])
	if !ok {
		if m[3] == "" {
			return 0, errNoUnit
		}
		return 0, fmt.Errorf("unknown unit %q", m[3])
	}
	num := digitsToInt(m[1] + m[2])
	num.Mul(num, big.NewInt(mult))
	den := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(len(m[2]))), nil)
	quo, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	if rem.Sign() != 0 {
		return 0, errNotWhole
	}
	if !quo.IsInt64() || quo.Int64() == math.MaxInt64 {
		return 0, errTooLarge
	}
	return quo.Int64(), nil
}

// digitsToInt converts a string of decimal digits to a big integer.
func digitsToInt(digits string) *big.Int {
	n := new(big.Int)
	ten := big.NewInt(10)
	for _, c := range digits {
		n.Mul(n, ten)
		n.Add(n, big.NewInt(int64(c-'0')))
	}
	return n
}

// Size is a number of bytes. In YAML it is written as a whole number of bytes,
// as a number followed by KiB, MiB, GiB or TiB, or as the word "unlimited".
type Size int64

// UnlimitedSize is the Size that means no limit.
const UnlimitedSize Size = math.MaxInt64

const sizeAccepted = `use a whole number of bytes, a number followed by KiB, MiB, GiB or TiB, or "unlimited"`

// sizeMultiplier returns the number of bytes in a size unit. The empty unit
// means bytes.
func sizeMultiplier(unit string) (int64, bool) {
	switch unit {
	case "":
		return 1, true
	case "KiB":
		return 1 << 10, true
	case "MiB":
		return 1 << 20, true
	case "GiB":
		return 1 << 30, true
	case "TiB":
		return 1 << 40, true
	}
	return 0, false
}

// Unlimited reports whether s is UnlimitedSize.
func (s Size) Unlimited() bool { return s == UnlimitedSize }

// UnmarshalText parses a size such as "5MiB", "1.5GiB", "4096" or "unlimited".
func (s *Size) UnmarshalText(text []byte) error {
	value := string(text)
	if value == unlimitedWord {
		*s = UnlimitedSize
		return nil
	}
	n, err := parseScaled(value, sizeMultiplier)
	if err != nil {
		return fmt.Errorf("invalid size %q: %w (%s)", value, err, sizeAccepted)
	}
	*s = Size(n)
	return nil
}

// MarshalText renders the size as String does.
func (s Size) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// String renders the size in the largest binary unit that represents it
// exactly, for example "5MiB" or "1536KiB". A size that is not a multiple of
// 1 KiB is rendered as a plain number of bytes.
func (s Size) String() string {
	switch s {
	case UnlimitedSize:
		return unlimitedWord
	case 0:
		return "0"
	}
	units := []struct {
		suffix string
		size   Size
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}}
	for _, u := range units {
		if s%u.size == 0 {
			return strconv.FormatInt(int64(s/u.size), 10) + u.suffix
		}
	}
	return strconv.FormatInt(int64(s), 10)
}

// Duration is a length of time. In YAML it is written as a number followed by
// s, m, h or d (a day of 24 hours), as 0, or as the word "unlimited".
// Compound values such as "1h30m" are not accepted.
type Duration time.Duration

// UnlimitedDuration is the Duration that means no limit.
const UnlimitedDuration Duration = math.MaxInt64

const durationAccepted = `use a number followed by s, m, h or d, or 0, or "unlimited"`

// durationUnits lists the accepted suffixes, largest first.
var durationUnits = [...]struct {
	suffix string
	length time.Duration
}{{"d", 24 * time.Hour}, {"h", time.Hour}, {"m", time.Minute}, {"s", time.Second}}

// durationMultiplier returns the number of nanoseconds in a duration unit.
func durationMultiplier(unit string) (int64, bool) {
	for _, u := range durationUnits {
		if u.suffix == unit {
			return int64(u.length), true
		}
	}
	return 0, false
}

// Unlimited reports whether d is UnlimitedDuration.
func (d Duration) Unlimited() bool { return d == UnlimitedDuration }

// Std returns d as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// UnmarshalText parses a duration such as "30m", "1.5h", "7d", "0" or
// "unlimited".
func (d *Duration) UnmarshalText(text []byte) error {
	value := string(text)
	switch value {
	case unlimitedWord:
		*d = UnlimitedDuration
		return nil
	case "0":
		*d = 0
		return nil
	}
	n, err := parseScaled(value, durationMultiplier)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w (%s)", value, err, durationAccepted)
	}
	*d = Duration(n)
	return nil
}

// MarshalText renders the duration as String does.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// String renders the duration in the largest unit of d, h, m or s that
// represents it exactly, for example "7d" or "90m". A duration that is not a
// whole number of seconds is rendered as decimal seconds, for example "1.5s".
// A negative duration, which cannot be written in a file, is rendered by
// time.Duration.String.
func (d Duration) String() string {
	switch {
	case d == UnlimitedDuration:
		return unlimitedWord
	case d == 0:
		return "0"
	case d < 0:
		return time.Duration(d).String()
	}
	for _, u := range durationUnits {
		if time.Duration(d)%u.length == 0 {
			return strconv.FormatInt(int64(time.Duration(d)/u.length), 10) + u.suffix
		}
	}
	whole, frac := time.Duration(d)/time.Second, time.Duration(d)%time.Second
	digits := strings.TrimRight(fmt.Sprintf("%09d", int64(frac)), "0")
	return fmt.Sprintf("%d.%ss", int64(whole), digits)
}
