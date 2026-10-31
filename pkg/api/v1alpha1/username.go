package v1alpha1

import (
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
)

// ProvisionedLabel marks a User or PVC the operator created for a group
// member (XERK-2538). The provisioner never changes a User without it.
const ProvisionedLabel = "direwolf/provisioned"

// ProvisionedLabelValue is the value the operator sets for ProvisionedLabel.
const ProvisionedLabelValue = "true"

// UsernameAnnotation records the identity provider's username a provisioned
// User (and its claims) was created for. Two usernames can map to one object
// name ("Bob" and "bob"); the annotation keeps the second from taking the
// first one's User.
const UsernameAnnotation = "direwolf/username"

// MaxUserNameLen is the longest User name with spec.library (the CRD's rule):
// it goes into Library pod, Secret and catalogue App names.
const MaxUserNameLen = 40

// ErrInvalidUsername: a username with no valid User name.
var ErrInvalidUsername = errors.New("invalid username")

// UserNameFor maps an identity provider's username to its User's object name:
// ASCII letters lowercased, and refused unless that is a DNS-1123 label of at
// most MaxUserNameLen characters. Nothing else is rewritten ('@', '_', '.',
// or non-ASCII letters, which strings.ToLower would fold into ASCII: 'İ' and
// the Kelvin sign): a rewrite would map more usernames onto one name.
func UserNameFor(username string) (string, error) {
	name := strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, username)
	if len(name) > MaxUserNameLen {
		return "", fmt.Errorf("%w %q: longer than %d characters", ErrInvalidUsername, username, MaxUserNameLen)
	}
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
		return "", fmt.Errorf("%w %q: %s", ErrInvalidUsername, username, strings.Join(errs, "; "))
	}
	return name, nil
}

// IsFor reports whether u is username's User. A provisioned User (with
// UsernameAnnotation) is only the username it was created for; any other
// only its exact name, as before mapping existed, so "Bob" never gets a
// hand-written "bob".
func (u *User) IsFor(username string) bool {
	if raw, ok := u.Annotations[UsernameAnnotation]; ok {
		return raw == username
	}
	return u.Name == username
}

// LookupUser finds username's User through get (a lister's or client's Get).
// A username with no valid name, or whose name belongs to another username,
// is NotFound, like a missing User. Every place that turns a signed-in
// username into a User must use it, so they agree with the provisioner.
func LookupUser(get func(name string) (*User, error), username string) (*User, error) {
	name, err := UserNameFor(username)
	if err != nil {
		return nil, apierrors.NewNotFound(Resource("users"), username)
	}
	user, err := get(name)
	if err != nil {
		return nil, err
	}
	if !user.IsFor(username) {
		return nil, apierrors.NewNotFound(Resource("users"), username)
	}
	return user, nil
}
