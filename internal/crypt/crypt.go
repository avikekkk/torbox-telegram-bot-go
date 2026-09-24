// Package crypt reproduces the AES-CTR obfuscation the Python bot used to turn
// an indexer's result GUID into the short NZB ID shown in search results.
//
// The scheme has to stay bit-compatible with pyaes: AESModeOfOperationCTR
// defaults to a Counter whose initial value is 1, rendered as a 16-byte
// big-endian block. That makes the IV fifteen zero bytes followed by 0x01.
package crypt

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"unicode/utf8"
)

var key = []byte{
	0x24, 0xeb, 0xb1, 0x72, 0x14, 0xf2, 0xfe, 0xa6,
	0x34, 0x0a, 0xc3, 0xb7, 0x14, 0xb7, 0xe2, 0xbf,
	0xa8, 0x58, 0xec, 0x5c, 0x77, 0xa2, 0xab, 0xdb,
	0x7d, 0xe2, 0x44, 0x96, 0xc9, 0xe7, 0x2f, 0x73,
}

var iv = []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}

func stream() (cipher.Stream, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewCTR(block, iv), nil
}

// Encrypt returns the base64 of the ciphertext.
func Encrypt(text string) (string, error) {
	s, err := stream()
	if err != nil {
		return "", err
	}
	out := make([]byte, len(text))
	s.XORKeyStream(out, []byte(text))
	return base64.StdEncoding.EncodeToString(out), nil
}

// Decrypt reverses Encrypt.
func Decrypt(text string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return "", err
	}
	s, err := stream()
	if err != nil {
		return "", err
	}
	out := make([]byte, len(raw))
	s.XORKeyStream(out, raw)
	if !utf8.Valid(out) {
		return "", errors.New("decoded bytes are not valid UTF-8")
	}
	return string(out), nil
}
