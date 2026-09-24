package crypt

import "testing"

// TestEncryptMatchesPyaes pins the ciphertext to what the Python bot produced,
// so NZB IDs from an older search still decode after the rewrite.
func TestEncryptMatchesPyaes(t *testing.T) {
	cases := map[string]string{
		"12345|nzbhydra": "PP1qP7iqyIXy3sokT5k=",
		"abc":            "bK06",
		// Longer than one AES block, so this also pins the counter increment.
		"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx": "dbchc/Wu3ofozss4RYARIeM7SeOoX+y2+iuCSw1/3JsHcg+T/WywNA==",
	}
	for plaintext, want := range cases {
		got, err := Encrypt(plaintext)
		if err != nil {
			t.Fatalf("Encrypt(%q): %v", plaintext, err)
		}
		if got != want {
			t.Errorf("Encrypt(%q) = %q, want %q", plaintext, got, want)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	const plaintext = "9f8e7d6c-1234|nzbhydra"
	encrypted, err := Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	decrypted, err := Decrypt(encrypted)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if decrypted != plaintext {
		t.Errorf("round trip = %q, want %q", decrypted, plaintext)
	}
}

func TestDecryptRejectsGarbage(t *testing.T) {
	for _, token := range []string{"not base64!!", "", "AAAAAAAAAAAAAAAAAAAAAAAA"} {
		if _, err := Decrypt(token); err == nil && token != "" {
			t.Errorf("Decrypt(%q) accepted a token it should not have", token)
		}
	}
}
