package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// loadSecret reads the HMAC secret (hex) from path, creating it with mode 0600 if absent.
func loadSecret(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		_, err = f.WriteString(hex.EncodeToString(raw) + "\n")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return raw, err
	}
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s must not be group/world accessible (chmod 600)", path)
	}
	raw, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(raw) < 16 {
		return nil, fmt.Errorf("%s: need >=16 bytes of hex", path)
	}
	return raw, nil
}

func mac(secret []byte, parts ...string) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(strings.Join(parts, "|")))
	return h.Sum(nil)
}

// identity derives the dummy account for a device id. Deterministic per (secret, dev).
func identity(secret []byte, dev string) (userID, serno, macAddr string) {
	m := mac(secret, "id", dev)
	uid := binary.BigEndian.Uint64(m[0:8])%9000000000000000 + 1000000000000000 // 16 digits: wfc-server rejects cfc > 9999999999999999
	serno = fmt.Sprintf("%09d", binary.BigEndian.Uint64(m[8:16])%1_000_000_000)
	macAddr = fmt.Sprintf("00:09:bf:%02x:%02x:%02x", m[16], m[17], m[18])
	return strconv.FormatUint(uid, 10), serno, macAddr
}

// vipCandidate returns the n-th candidate 10.77.A.B (A,B in 1..254) for dev; n>0 only on collision.
func vipCandidate(secret []byte, dev string, n int) [4]byte {
	m := mac(secret, "vip", dev, strconv.Itoa(n))
	return [4]byte{10, 77, 1 + m[0]%254, 1 + m[1]%254}
}
