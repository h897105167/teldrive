package gateway

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

// ticket_test.go —— 校验 ticket 的 JSON 契约与 Worker verifyTicket 的错误码映射。

func TestTicketSignVerifyRoundTripMatchesWorkerPayload(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	payload := Payload{
		Version: 1, Scope: ScopeDownload, FileID: "0f2c1d2e-0000-4000-8000-000000000000",
		Range: "0-9", Attachment: 1, UserID: 42, Nonce: "nonce", ExpiresAt: now.Add(time.Minute).Unix(),
	}
	token, err := Sign("secret", payload)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	segment, _, ok := strings.Cut(token, ".")
	if !ok {
		t.Fatalf("token %q has no separator", token)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		t.Fatalf("decode segment: %v", err)
	}
	want := `{"v":1,"s":"d","f":"0f2c1d2e-0000-4000-8000-000000000000","r":"0-9","a":1,"u":42,"n":"nonce","e":1700000060}`
	if string(decoded) != want {
		t.Fatalf("payload json = %s, want %s", decoded, want)
	}
	verified, err := Verify("secret", token, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if verified != payload {
		t.Fatalf("Verify = %+v, want %+v", verified, payload)
	}
}

func TestTicketSignOmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()
	token, err := Sign("secret", Payload{Version: 1, Scope: ScopeDownload, FileID: "f", UserID: 7, Nonce: "n", ExpiresAt: 99})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	segment, _, _ := strings.Cut(token, ".")
	decoded, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		t.Fatalf("decode segment: %v", err)
	}
	if want := `{"v":1,"s":"d","f":"f","u":7,"n":"n","e":99}`; string(decoded) != want {
		t.Fatalf("payload json = %s, want %s", decoded, want)
	}
}

func TestTicketVerifyRejectsTamperingAndWrongSecret(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	token, err := Sign("secret", Payload{Version: 1, Scope: ScopeDownload, FileID: "f", UserID: 1, Nonce: "n", ExpiresAt: now.Add(time.Hour).Unix()})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	segment, signature, _ := strings.Cut(token, ".")

	tests := map[string]struct {
		token  string
		secret string
	}{
		"tampered signature": {token: segment + "." + signature[:len(signature)-1] + "X", secret: "secret"},
		"tampered segment":   {token: "A" + segment[1:] + "." + signature, secret: "secret"},
		"wrong secret":       {token: token, secret: "other-secret"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := Verify(test.secret, test.token, now); !errors.Is(err, ErrTicketSignatureInvalid) {
				t.Fatalf("Verify = %v, want ErrTicketSignatureInvalid", err)
			}
		})
	}
}

func TestTicketVerifyMalformed(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	for _, token := range []string{"", "no-separator", ".onlysignature", "onlysegment."} {
		if _, err := Verify("secret", token, now); !errors.Is(err, ErrTicketMalformed) {
			t.Fatalf("Verify(%q) = %v, want ErrTicketMalformed", token, err)
		}
	}
}

func TestTicketVerifyPayloadInvalid(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	tests := map[string]Payload{
		"wrong version": {Version: 2, Scope: ScopeDownload, FileID: "f", UserID: 1, Nonce: "n", ExpiresAt: now.Unix() + 60},
		"unknown scope": {Version: 1, Scope: "x", FileID: "f", UserID: 1, Nonce: "n", ExpiresAt: now.Unix() + 60},
		"empty file id": {Version: 1, Scope: ScopeUpload, UserID: 1, Nonce: "n", ExpiresAt: now.Unix() + 60},
	}
	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			token, err := Sign("secret", payload)
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}
			if _, err := Verify("secret", token, now); !errors.Is(err, ErrTicketPayloadInvalid) {
				t.Fatalf("Verify = %v, want ErrTicketPayloadInvalid", err)
			}
		})
	}
}

func TestTicketVerifyExpired(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	token, err := Sign("secret", Payload{Version: 1, Scope: ScopeDownload, FileID: "f", UserID: 1, Nonce: "n", ExpiresAt: now.Unix()})
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := Verify("secret", token, now); !errors.Is(err, ErrTicketExpired) {
		t.Fatalf("Verify = %v, want ErrTicketExpired", err)
	}
}

func TestTicketSignVerifyUploadRoundTrip(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	payload := Payload{
		Version: 1, Scope: ScopeUpload, FileID: "9a0c1d2e-0000-4000-8000-000000000000",
		UserID: 42, Nonce: "nonce", ExpiresAt: now.Add(time.Minute).Unix(),
		PartNo: 3, BotSlot: 2, PartBytes: 16 << 20, LeaseToken: "7b1c1d2e-0000-4000-8000-000000000000",
	}
	token, err := Sign("secret", payload)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	segment, _, _ := strings.Cut(token, ".")
	decoded, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		t.Fatalf("decode segment: %v", err)
	}
	want := `{"v":1,"s":"u","f":"9a0c1d2e-0000-4000-8000-000000000000","u":42,"n":"nonce","e":1700000060,"p":3,"b":2,"z":16777216,"l":"7b1c1d2e-0000-4000-8000-000000000000"}`
	if string(decoded) != want {
		t.Fatalf("payload json = %s, want %s", decoded, want)
	}
	verified, err := Verify("secret", token, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if verified != payload {
		t.Fatalf("Verify = %+v, want %+v", verified, payload)
	}
}

func TestTicketVerifyRejectsNonNumericExpiry(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	segments := []string{
		`{"v":1,"s":"d","f":"f","u":1,"n":"n","e":"9999999999"}`,
		`{"v":1,"s":"d","f":"f","u":1,"n":"n","e":null}`,
		`{"v":1,"s":"d","f":"f","u":1,"n":"n"}`,
	}
	for _, payload := range segments {
		segment := base64.RawURLEncoding.EncodeToString([]byte(payload))
		token := segment + "." + signSegment("secret", segment)
		if _, err := Verify("secret", token, now); !errors.Is(err, ErrTicketPayloadInvalid) {
			t.Fatalf("Verify(%s) = %v, want ErrTicketPayloadInvalid", payload, err)
		}
	}
}
