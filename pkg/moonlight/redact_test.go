package moonlight

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

func TestRedactedQueryHidesKeyMaterial(t *testing.T) {
	q, err := url.ParseQuery("appid=firefox&rikey=773448F67992470C5C62848D361E1025&rikeyid=1311662065&clientpairingsecret=abc&salt=s")
	if err != nil {
		t.Fatal(err)
	}
	logged := fmt.Sprint(redactedQuery(q))
	for _, secret := range []string{"773448F67992470C5C62848D361E1025", "1311662065", "abc", "salt:[s]"} {
		if strings.Contains(logged, secret) {
			t.Errorf("logged query %s contains %q", logged, secret)
		}
	}
	if !strings.Contains(logged, "appid:[firefox]") {
		t.Errorf("logged query %s lost a non-secret parameter", logged)
	}
	if q.Get("rikey") != "773448F67992470C5C62848D361E1025" {
		t.Error("redaction mutated the request's query")
	}
}
