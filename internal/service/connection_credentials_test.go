package service

import (
	"strings"
	"testing"
)

func TestCredentialsFingerprintChangesWhenACredentialChanges(t *testing.T) {
	before := credentialsFingerprint(map[string][]byte{"password": []byte("old")})
	after := credentialsFingerprint(map[string][]byte{"password": []byte("new")})

	if before == after {
		t.Errorf("rotating the password left the fingerprint at %q; nothing downstream would notice", before)
	}
}

// The fingerprint lands in spec.values, so an unstable one would rewrite the
// Connection on every reconcile and restart consumers for nothing.
func TestCredentialsFingerprintIsStableAcrossCalls(t *testing.T) {
	secrets := map[string][]byte{"accessKey": []byte("AKIA"), "secretKey": []byte("s3cret")}

	first := credentialsFingerprint(secrets)
	for range 20 {
		if got := credentialsFingerprint(secrets); got != first {
			t.Fatalf("fingerprint varies between calls: %q then %q", first, got)
		}
	}
}

// Two fields whose values are swapped must not collide.
func TestCredentialsFingerprintDistinguishesFieldsFromValues(t *testing.T) {
	one := credentialsFingerprint(map[string][]byte{"user": []byte("a"), "password": []byte("b")})
	two := credentialsFingerprint(map[string][]byte{"user": []byte("b"), "password": []byte("a")})

	if one == two {
		t.Error("swapping two credential values produced the same fingerprint")
	}
}

// The whole point of keeping credentials out of the CRD would be lost if the
// fingerprint carried them.
func TestCredentialsFingerprintDisclosesNothing(t *testing.T) {
	fingerprint := credentialsFingerprint(map[string][]byte{"password": []byte("hunter2")})

	if strings.Contains(fingerprint, "hunter2") {
		t.Errorf("the fingerprint %q contains the credential", fingerprint)
	}
	if len(fingerprint) != 12 {
		t.Errorf("fingerprint length = %d, want 12", len(fingerprint))
	}
}

func TestCredentialsFingerprintIsEmptyWithoutCredentials(t *testing.T) {
	if got := credentialsFingerprint(nil); got != "" {
		t.Errorf("got %q, want empty — an internal connection has no credentials", got)
	}
}
