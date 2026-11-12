package v1alpha1

import (
	"regexp"
	"strings"
	"unicode"
)

// wivrnKeyPattern is the CRD's publicKey pattern: WiVRn's stored form.
var wivrnKeyPattern = regexp.MustCompile(`^([A-Za-z0-9+/]{4})*([A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$`)

// IsWiVRnKey is whether key is in WiVRn's stored form, as HeadsetSpec.PublicKey admits it.
func IsWiVRnKey(key string) bool {
	return len(key) >= 4 && len(key) <= 1024 && wivrnKeyPattern.MatchString(key)
}

// HeadsetNameMaxLen is the CRDs' limit on a headset's name. OpenAPI's
// maxLength counts characters (code points), not bytes.
const HeadsetNameMaxLen = 128

// CleanHeadsetName is a headset's own name as a Headset's displayName: valid
// UTF-8 with no control, format (bidi) or line/paragraph-separator runes, cut
// to the CRD's 128 characters.
func CleanHeadsetName(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(name, ""))
	if r := []rune(name); len(r) > HeadsetNameMaxLen {
		name = string(r[:HeadsetNameMaxLen])
	}
	return name
}
