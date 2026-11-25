package identity

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHashPasswordRoundTrip(t *testing.T) {
	password := "correct horse battery staple"
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("unexpected PHC header: %q", hash)
	}
	if ok, err := VerifyPassword(hash, password); err != nil || !ok {
		t.Fatalf("verify correct password = %v, %v; want true, nil", ok, err)
	}
	if ok, err := VerifyPassword(hash, password+"!"); err != nil || ok {
		t.Fatalf("verify wrong password = %v, %v; want false, nil", ok, err)
	}
	if stale, err := NeedsRehash(hash); err != nil || stale {
		t.Fatalf("NeedsRehash fresh hash = %v, %v; want false, nil", stale, err)
	}
}

func TestHashPasswordUsesRandomSalt(t *testing.T) {
	first, err := HashPassword("same password every time")
	if err != nil {
		t.Fatalf("first hash: %v", err)
	}
	second, err := HashPassword("same password every time")
	if err != nil {
		t.Fatalf("second hash: %v", err)
	}
	if first == second {
		t.Fatal("two hashes of the same password must differ")
	}
}

func TestVerifyPasswordRejectsMalformedHash(t *testing.T) {
	cases := map[string]string{
		"empty":           "",
		"not phc":         "not-a-hash",
		"wrong algorithm": "$argon2i$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA",
		"wrong version":   "$argon2id$v=16$m=65536,t=3,p=2$c2FsdA$aGFzaA",
		"bad params":      "$argon2id$v=19$m=abc,t=3,p=2$c2FsdA$aGFzaA",
		"absurd memory":   "$argon2id$v=19$m=999999999,t=3,p=2$c2FsdA$aGFzaA",
		"bad salt base64": "$argon2id$v=19$m=65536,t=3,p=2$!!!!$aGFzaA",
		"missing key":     "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA",
		"empty key":       "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$",
		"oversized input": "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA",
	}
	for name, hash := range cases {
		t.Run(name, func(t *testing.T) {
			password := "x"
			if name == "oversized input" {
				password = strings.Repeat("x", maxVerifyPasswordLength+1)
			}
			_, err := VerifyPassword(hash, password)
			if errors.Is(err, ErrInvalidPasswordHash) ||
				(name == "oversized input" && errors.Is(err, ErrPasswordTooLong)) {
				return
			}
			t.Fatalf("VerifyPassword error = %v, want a rejection", err)
		})
	}
}

func TestPasswordPolicy(t *testing.T) {
	tooLong := strings.Repeat("a", MaxPasswordLength+1)
	cases := []struct {
		name     string
		policy   PasswordPolicy
		username string
		password string
		want     error
	}{
		{name: "default accepts twelve lowercase bytes", policy: DefaultPasswordPolicy(), password: "abcdefghijkl"},
		{name: "default rejects eleven bytes", policy: DefaultPasswordPolicy(), password: "abcdefghijk", want: ErrPasswordTooShort},
		{name: "configurable minimum", policy: PasswordPolicy{MinLength: 8}, password: "abcdefgh"},
		{name: "minimum below range invalid", policy: PasswordPolicy{MinLength: 7}, password: "abcdefghijkl", want: ErrInvalidPasswordPolicy},
		{name: "minimum above range invalid", policy: PasswordPolicy{MinLength: 129}, password: "abcdefghijkl", want: ErrInvalidPasswordPolicy},
		{name: "rejects over 128 bytes", policy: PasswordPolicy{MinLength: 8}, password: tooLong, want: ErrPasswordTooLong},
		{name: "rejects username", policy: DefaultPasswordPolicy(), username: "alexander-smith", password: "alexander-smith", want: ErrPasswordMatchesUsername},
		{name: "accepts password containing username as substring", policy: DefaultPasswordPolicy(), username: "alice", password: "alice-writes-code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.policy.Validate(tc.username, tc.password)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Validate error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestNeedsRehashAndRehashAfterParameterUpgrade(t *testing.T) {
	oldValue := "an-old-but-valid-phrase"

	old := argonParams{memory: 8 * 1024, time: 1, threads: 1, keyLen: argonKeyLength}
	salt := bytes.Repeat([]byte{0x2a}, argonSaltLength)
	oldHash := encodeHash(old, salt, deriveKey([]byte(oldValue), salt, old))

	stale, err := NeedsRehash(oldHash)
	if err != nil || !stale {
		t.Fatalf("NeedsRehash old hash = %v, %v; want true, nil", stale, err)
	}
	if ok, err := VerifyPassword(oldHash, oldValue); err != nil || !ok {
		t.Fatalf("verify old hash = %v, %v; want true, nil", ok, err)
	}

	newHash, err := RehashPassword(oldValue)
	if err != nil {
		t.Fatalf("rehash: %v", err)
	}
	if ok, err := VerifyPassword(newHash, oldValue); err != nil || !ok {
		t.Fatalf("verify rehashed password = %v, %v; want true, nil", ok, err)
	}
	if stale, err := NeedsRehash(newHash); err != nil || stale {
		t.Fatalf("NeedsRehash new hash = %v, %v; want false, nil", stale, err)
	}
	params, _, _, err := parseHash(newHash)
	if err != nil || params.memory != argonMemory || params.time != argonTime || params.threads != argonThreads {
		t.Fatalf("rehashed params = %+v, %v; want current params", params, err)
	}
}

func TestNeedsRehashRejectsMalformedHash(t *testing.T) {
	if _, err := NeedsRehash("garbage"); !errors.Is(err, ErrInvalidPasswordHash) {
		t.Fatalf("NeedsRehash error = %v, want ErrInvalidPasswordHash", err)
	}
}

func TestFakePasswordHash(t *testing.T) {
	fake := FakePasswordHash()
	if fake != FakePasswordHash() {
		t.Fatal("FakePasswordHash must be stable")
	}
	if ok, err := VerifyPassword(fake, "not-the-placeholder"); err != nil || ok {
		t.Fatalf("verify fake hash = %v, %v; want false, nil", ok, err)
	}
	if stale, err := NeedsRehash(fake); err != nil || stale {
		t.Fatalf("fake hash must use current parameters: %v, %v", stale, err)
	}
}

func TestFakePasswordHashTiming(t *testing.T) {
	realHash, err := HashPassword("a-realistic-password")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	measure := func(hash string) time.Duration {
		var total time.Duration
		for i := 0; i < 2; i++ {
			start := time.Now()
			if _, err := VerifyPassword(hash, "wrong-password-value"); err != nil {
				t.Fatalf("verify: %v", err)
			}
			total += time.Since(start)
		}
		return total / 2
	}
	real := measure(realHash)
	fake := measure(FakePasswordHash())
	if real <= 0 || fake <= 0 {
		t.Fatalf("unexpected durations: real=%v fake=%v", real, fake)
	}
	// Generous ratio: the goal is to catch a fake hash that skips argon2
	// entirely, not to measure precisely on a loaded CI machine.
	if real > fake*8 || fake > real*8 {
		t.Fatalf("timing differs too much: real=%v fake=%v", real, fake)
	}
}

func TestHashConcurrencyIsBounded(t *testing.T) {
	if cap(hashSemaphore) != maxHashConcurrency {
		t.Fatalf("semaphore capacity = %d, want %d", cap(hashSemaphore), maxHashConcurrency)
	}
	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hash, err := HashPassword("parallel-password")
			if err != nil {
				errs <- err
				return
			}
			ok, err := VerifyPassword(hash, "parallel-password")
			if err != nil || !ok {
				errs <- errors.New("concurrent hash failed verification")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
