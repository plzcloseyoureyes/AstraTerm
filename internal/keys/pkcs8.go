package keys

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/des"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"hash"
)

// Encrypted PKCS#8 ("ENCRYPTED PRIVATE KEY", RFC 5958 / PKCS#5 v2.1 PBES2) — produced by `openssl genpkey -aes256`,
// `ssh-keygen -m PKCS8` and many tools, but not readable by x/crypto/ssh. Supported: PBKDF2 with HMAC-SHA1/224/256/
// 384/512 and AES-128/192/256-CBC or 3DES-CBC.

var (
	oidPBES2          = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 13}
	oidPBKDF2         = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 12}
	oidHMACWithSHA1   = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 7}
	oidHMACWithSHA224 = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 8}
	oidHMACWithSHA256 = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 9}
	oidHMACWithSHA384 = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 10}
	oidHMACWithSHA512 = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 11}
	oidAES128CBC      = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 2}
	oidAES192CBC      = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 22}
	oidAES256CBC      = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 42}
	oidDESEDE3CBC     = asn1.ObjectIdentifier{1, 2, 840, 113549, 3, 7}
)

const (
	// pkcs8Iterations is the PBKDF2-HMAC-SHA256 cost of exported keys.
	pkcs8Iterations = 200_000
	// maxPKCS8Iterations bounds the cost of imported keys.
	maxPKCS8Iterations = 5_000_000
)

type encryptedPrivateKeyInfo struct {
	Algorithm     pkix.AlgorithmIdentifier
	EncryptedData []byte
}

type pbes2Params struct {
	KeyDerivationFunc pkix.AlgorithmIdentifier
	EncryptionScheme  pkix.AlgorithmIdentifier
}

type pbkdf2Params struct {
	Salt           []byte
	IterationCount int
	KeyLength      int                      `asn1:"optional"`
	PRF            pkix.AlgorithmIdentifier `asn1:"optional"`
}

// decryptPKCS8 decrypts and parses an EncryptedPrivateKeyInfo.
func decryptPKCS8(der, passphrase []byte) (crypto.PrivateKey, error) {
	var info encryptedPrivateKeyInfo
	if rest, err := asn1.Unmarshal(der, &info); err != nil || len(rest) > 0 {
		return nil, invalidKey("invalid encrypted PKCS#8 key")
	}
	if !info.Algorithm.Algorithm.Equal(oidPBES2) {
		return nil, unsupportedKey(fmt.Sprintf("unsupported PKCS#8 encryption %v (only PBES2 is supported)", info.Algorithm.Algorithm))
	}
	var params pbes2Params
	if _, err := asn1.Unmarshal(info.Algorithm.Parameters.FullBytes, &params); err != nil {
		return nil, invalidKey("invalid PBES2 parameters")
	}
	if !params.KeyDerivationFunc.Algorithm.Equal(oidPBKDF2) {
		return nil, unsupportedKey(fmt.Sprintf("unsupported PKCS#8 key derivation %v (only PBKDF2 is supported)", params.KeyDerivationFunc.Algorithm))
	}
	var kdf pbkdf2Params
	if _, err := asn1.Unmarshal(params.KeyDerivationFunc.Parameters.FullBytes, &kdf); err != nil {
		return nil, invalidKey("invalid PBKDF2 parameters")
	}
	if kdf.IterationCount < 1 || kdf.IterationCount > maxPKCS8Iterations {
		return nil, unsupportedKey(fmt.Sprintf("the key derivation uses %d iterations (at most %d are supported)", kdf.IterationCount, maxPKCS8Iterations))
	}
	newHash, err := prfHash(kdf.PRF.Algorithm)
	if err != nil {
		return nil, err
	}
	var (
		keyLen   int
		newBlock func([]byte) (cipher.Block, error)
	)
	switch alg := params.EncryptionScheme.Algorithm; {
	case alg.Equal(oidAES128CBC):
		keyLen, newBlock = 16, aes.NewCipher
	case alg.Equal(oidAES192CBC):
		keyLen, newBlock = 24, aes.NewCipher
	case alg.Equal(oidAES256CBC):
		keyLen, newBlock = 32, aes.NewCipher
	case alg.Equal(oidDESEDE3CBC):
		keyLen, newBlock = 24, des.NewTripleDESCipher
	default:
		return nil, unsupportedKey(fmt.Sprintf("unsupported PKCS#8 cipher %v", alg))
	}
	if kdf.KeyLength != 0 && kdf.KeyLength != keyLen {
		return nil, invalidKey("invalid PBKDF2 key length")
	}
	var iv []byte
	if _, err := asn1.Unmarshal(params.EncryptionScheme.Parameters.FullBytes, &iv); err != nil {
		return nil, invalidKey("invalid cipher parameters")
	}
	var key []byte
	err = withKDF(func() (err error) {
		key, err = pbkdf2.Key(newHash, string(passphrase), kdf.Salt, kdf.IterationCount, keyLen)
		return err
	})
	if err != nil {
		if isKDFBusy(err) {
			return nil, err
		}
		return nil, invalidKey("invalid PBKDF2 parameters")
	}
	block, err := newBlock(key)
	wipe(key)
	if err != nil {
		return nil, err
	}
	bs := block.BlockSize()
	if len(iv) != bs || len(info.EncryptedData) == 0 || len(info.EncryptedData)%bs != 0 {
		return nil, invalidKey("invalid encrypted PKCS#8 data")
	}
	plain := make([]byte, len(info.EncryptedData))
	defer wipe(plain) // the parsed key copies what it keeps
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, info.EncryptedData)
	plain, ok := pkcs7Unpad(plain, bs)
	if !ok {
		return nil, &passphraseError{wrong: true}
	}
	raw, err := x509.ParsePKCS8PrivateKey(plain)
	if err != nil {
		return nil, &passphraseError{wrong: true}
	}
	return raw, nil
}

func prfHash(oid asn1.ObjectIdentifier) (func() hash.Hash, error) {
	switch {
	case len(oid) == 0, oid.Equal(oidHMACWithSHA1):
		return sha1.New, nil
	case oid.Equal(oidHMACWithSHA224):
		return sha256.New224, nil
	case oid.Equal(oidHMACWithSHA256):
		return sha256.New, nil
	case oid.Equal(oidHMACWithSHA384):
		return sha512.New384, nil
	case oid.Equal(oidHMACWithSHA512):
		return sha512.New, nil
	}
	return nil, unsupportedKey(fmt.Sprintf("unsupported PBKDF2 hash %v", oid))
}

func pkcs7Unpad(b []byte, blockSize int) ([]byte, bool) {
	if len(b) == 0 {
		return nil, false
	}
	n := int(b[len(b)-1])
	if n == 0 || n > blockSize || n > len(b) {
		return nil, false
	}
	for _, c := range b[len(b)-n:] {
		if int(c) != n {
			return nil, false
		}
	}
	return b[:len(b)-n], true
}

// encryptPKCS8 encodes raw as an "ENCRYPTED PRIVATE KEY" (PBES2: PBKDF2-HMAC-SHA256 + AES-256-CBC).
func encryptPKCS8(raw crypto.PrivateKey, passphrase []byte) (*pem.Block, error) {
	der, err := x509.MarshalPKCS8PrivateKey(raw)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}
	key, err := pbkdf2.Key(sha256.New, string(passphrase), salt, pkcs8Iterations, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	pad := aes.BlockSize - len(der)%aes.BlockSize
	plain := append(append([]byte(nil), der...), make([]byte, pad)...)
	for i := len(der); i < len(plain); i++ {
		plain[i] = byte(pad)
	}
	ct := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, plain)

	kdfParams, err := asn1.Marshal(pbkdf2Params{
		Salt:           salt,
		IterationCount: pkcs8Iterations,
		PRF:            pkix.AlgorithmIdentifier{Algorithm: oidHMACWithSHA256, Parameters: asn1.NullRawValue},
	})
	if err != nil {
		return nil, err
	}
	ivParam, err := asn1.Marshal(iv)
	if err != nil {
		return nil, err
	}
	p2, err := asn1.Marshal(pbes2Params{
		KeyDerivationFunc: pkix.AlgorithmIdentifier{Algorithm: oidPBKDF2, Parameters: asn1.RawValue{FullBytes: kdfParams}},
		EncryptionScheme:  pkix.AlgorithmIdentifier{Algorithm: oidAES256CBC, Parameters: asn1.RawValue{FullBytes: ivParam}},
	})
	if err != nil {
		return nil, err
	}
	out, err := asn1.Marshal(encryptedPrivateKeyInfo{
		Algorithm:     pkix.AlgorithmIdentifier{Algorithm: oidPBES2, Parameters: asn1.RawValue{FullBytes: p2}},
		EncryptedData: ct,
	})
	if err != nil {
		return nil, err
	}
	return &pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: out}, nil
}
