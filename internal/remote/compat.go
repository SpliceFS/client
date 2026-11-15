package remote

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	// serverVersionHeader carries the version of splicefsd on every response it sends. It is
	// written in the canonical form Go uses; header names are not case sensitive, so this is
	// the same header as X-SpliceFS-Server-Version.
	serverVersionHeader = "X-Splicefs-Server-Version"
	// firstVersionWithHeader is the first splicefsd that sends serverVersionHeader. A
	// node that answers without it is older than this.
	firstVersionWithHeader = "0.4.0"
)

// release is a version of the form MAIN.MINOR or MAIN.MINOR.FIX, as parsed from a version
// string. text is how it is shown to people: no leading v, nothing after the last number.
type release struct {
	main  int
	minor int
	text  string
}

// incompatibleError is the text of a refusal that matches ErrIncompatible. The sentinel is
// the cause and not part of the text, because the text already says everything a person
// needs and a prefix would only repeat it.
type incompatibleError struct {
	message string
}

func (e incompatibleError) Error() string {
	return e.message
}

func (incompatibleError) Unwrap() error {
	return ErrIncompatible
}

// parseRelease reads a version such as "v0.4.4", "0.4" or "v0.4.4-3-gabc123-dirty". It
// reports false for anything that does not start with MAIN.MINOR, such as "dev" or a bare
// commit hash.
func parseRelease(s string) (release, bool) {
	first, ok := leadingNumber(strings.TrimPrefix(s, "v"))
	if !ok {
		return release{}, false
	}
	afterDot, ok := strings.CutPrefix(first.rest, ".")
	if !ok {
		return release{}, false
	}
	second, ok := leadingNumber(afterDot)
	if !ok {
		return release{}, false
	}
	parsed := release{main: first.value, minor: second.value}
	parsed.text = fmt.Sprintf("%d.%d", parsed.main, parsed.minor)
	if afterMinor, found := strings.CutPrefix(second.rest, "."); found {
		if fix, hasFix := leadingNumber(afterMinor); hasFix {
			parsed.text += "." + strconv.Itoa(fix.value)
		}
	}
	return parsed, true
}

// numberPrefix is a decimal number and what follows it in the text it was read from.
type numberPrefix struct {
	value int
	rest  string
}

// leadingNumber reads the decimal number that text starts with. It reports false when text does
// not start with a digit or the number does not fit an int.
func leadingNumber(text string) (numberPrefix, bool) {
	end := strings.IndexFunc(text, func(r rune) bool { return r < '0' || r > '9' })
	if end < 0 {
		end = len(text)
	}
	if end == 0 {
		return numberPrefix{}, false
	}
	value, err := strconv.Atoi(text[:end])
	if err != nil {
		return numberPrefix{}, false
	}
	return numberPrefix{value: value, rest: text[end:]}, true
}

// checkVersion decides whether the answer of the node at host may be used by a client of
// version clientVersion. reported holds the values of the version header, status is the
// status of the response and want the status the call wanted.
//
// A client without a MAIN.MINOR version, such as a development build, checks nothing. A
// header that is not a version, such as the one of a development server, is accepted. An
// answer without the header is blamed on the node only when it is the answer the call
// wanted: an error page of a proxy has no such header and says nothing about the node.
func checkVersion(host, clientVersion string, reported []string, status, want int) error {
	own, ok := parseRelease(clientVersion)
	if !ok {
		return nil
	}
	if len(reported) == 0 {
		if status != want {
			return nil
		}
		return incompatibleError{fmt.Sprintf(
			"server %s does not report its version, so it is older than %s, and this client (%s) "+
				"needs splicefsd %d.%d or newer: upgrade the server, or install an older client",
			host, firstVersionWithHeader, own.text, own.main, own.minor)}
	}
	server, ok := parseRelease(reported[0])
	if !ok {
		return nil
	}
	switch {
	case server.main != own.main:
		return incompatibleError{fmt.Sprintf(
			"server %s runs splicefsd %s and this client is %s: "+
				"client and server must have the same first version number",
			host, server.text, own.text)}
	case server.minor < own.minor:
		return incompatibleError{fmt.Sprintf(
			"server %s runs splicefsd %s, and this client (%s) needs splicefsd %d.%d or newer: "+
				"upgrade the server, or install an older client",
			host, server.text, own.text, own.main, own.minor)}
	default:
		return nil
	}
}
