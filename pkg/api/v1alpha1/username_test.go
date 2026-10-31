package v1alpha1

import (
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestUserNameFor(t *testing.T) {
	for username, want := range map[string]string{
		"kim": "kim", "Kim": "kim", "KIM-2": "kim-2", "0day": "0day",
		"a234567890123456789012345678901234567890": "a234567890123456789012345678901234567890",
		// Refused, never rewritten: a rewrite maps more usernames onto one name.
		"": "", "kim@example.com": "", "kim_doe": "", "kim.doe": "", "-kim": "", "kim-": "", "ki m": "",
		"a234567890123456789012345678901234567890x": "", "kİm": "", "\u212aim": "",
	} {
		got, err := UserNameFor(username)
		if want == "" {
			if !errors.Is(err, ErrInvalidUsername) {
				t.Errorf("UserNameFor(%q) = %q, %v; want refused", username, got, err)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("UserNameFor(%q) = %q, %v; want %q", username, got, err, want)
		}
	}
}

func TestLookupUser(t *testing.T) {
	users := map[string]*User{
		// Hand-written: only its exact name.
		"sam": {ObjectMeta: metav1.ObjectMeta{Name: "sam"}},
		// Provisioned for "Kim": only that username.
		"kim": {ObjectMeta: metav1.ObjectMeta{Name: "kim", Annotations: map[string]string{UsernameAnnotation: "Kim"}}},
	}
	get := func(name string) (*User, error) {
		if u, ok := users[name]; ok {
			return u, nil
		}
		return nil, apierrors.NewNotFound(Resource("users"), name)
	}
	for username, want := range map[string]string{
		"sam": "sam", "Sam": "", "SAM": "",
		"Kim": "kim", "kim": "", "KIM": "",
		"alex": "", "": "", "sam@x": "",
	} {
		u, err := LookupUser(get, username)
		if want == "" {
			if !apierrors.IsNotFound(err) {
				t.Errorf("LookupUser(%q) = %v, %v; want NotFound", username, u, err)
			}
			continue
		}
		if err != nil || u.Name != want {
			t.Errorf("LookupUser(%q) = %v, %v; want %s", username, u, err, want)
		}
	}
}
