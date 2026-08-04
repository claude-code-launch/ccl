package oauthproxy

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5" // #nosec G501 -- Qoder's wire protocol requires MD5 signatures.
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	qoderIDEVersion      = "1.1.3"
	qoderClientType      = "5"
	qoderDataPolicy      = "disagree"
	qoderLoginVersion    = "v2"
	qoderMachineType     = "5"
	qoderMaxBodyBytes    = int64(128 << 20)
	qoderMaxErrorBytes   = int64(1 << 20)
	qoderStandardBase64  = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	qoderCustomBase64    = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
	qoderRSAPublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`
)

type qoderSigningCredential struct {
	userID      string
	accessToken string
	name        string
	email       string
	machineID   string
}

func qoderModelListURL() string {
	return strings.TrimRight(qoderAPIBaseURL, "/") + "/algo/api/v2/model/list?Encode=1"
}

func qoderChatURL() string {
	return strings.TrimRight(qoderAPIBaseURL, "/") + "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
}

func qoderRefreshURL() string {
	return strings.TrimRight(qoderCenterBaseURL, "/") + "/algo/api/v3/user/refresh_token"
}

func qoderEncodeBody(plaintext []byte) string {
	standard := base64.StdEncoding.EncodeToString(plaintext)
	n := len(standard)
	a := n / 3
	rearranged := standard[n-a:] + standard[a:n-a] + standard[:a]
	var encoded strings.Builder
	encoded.Grow(n)
	for _, character := range []byte(rearranged) {
		if character == '=' {
			encoded.WriteByte('$')
			continue
		}
		index := strings.IndexByte(qoderStandardBase64, character)
		if index >= 0 {
			encoded.WriteByte(qoderCustomBase64[index])
		} else {
			encoded.WriteByte(character)
		}
	}
	return encoded.String()
}

func qoderBuildAuthHeaders(body []byte, requestURL string, credential qoderSigningCredential) (http.Header, error) {
	if strings.TrimSpace(credential.userID) == "" {
		return nil, fmt.Errorf("Qoder user id is empty")
	}
	if strings.TrimSpace(credential.accessToken) == "" {
		return nil, fmt.Errorf("Qoder access token is empty")
	}
	aesKey, err := qoderAESKey()
	if err != nil {
		return nil, err
	}
	userInfo, err := json.Marshal(map[string]string{
		"uid":                  credential.userID,
		"security_oauth_token": credential.accessToken,
		"name":                 credential.name,
		"aid":                  "",
		"email":                credential.email,
	})
	if err != nil {
		return nil, fmt.Errorf("encode Qoder signed identity: %w", err)
	}
	encryptedUser, err := qoderAESEncrypt(userInfo, aesKey)
	if err != nil {
		return nil, err
	}
	publicKey, err := qoderPublicKey()
	if err != nil {
		return nil, err
	}
	encryptedKey, err := rsa.EncryptPKCS1v15(rand.Reader, publicKey, aesKey) // #nosec G403 -- required by Qoder's COSY protocol.
	if err != nil {
		return nil, fmt.Errorf("encrypt Qoder signing key: %w", err)
	}
	cosyKey := base64.StdEncoding.EncodeToString(encryptedKey)
	timestamp := fmt.Sprintf("%d", time.Now().Unix())
	requestID := uuid.NewString()
	payload, err := json.Marshal(map[string]string{
		"version":     "v1",
		"requestId":   requestID,
		"info":        base64.StdEncoding.EncodeToString(encryptedUser),
		"cosyVersion": qoderIDEVersion,
		"ideVersion":  "",
	})
	if err != nil {
		return nil, fmt.Errorf("encode Qoder authorization payload: %w", err)
	}
	payloadBase64 := base64.StdEncoding.EncodeToString(payload)
	sigPath, err := qoderSignaturePath(requestURL)
	if err != nil {
		return nil, err
	}
	signatureInput := payloadBase64 + "\n" + cosyKey + "\n" + timestamp + "\n" + string(body) + "\n" + sigPath
	signatureSum := md5.Sum([]byte(signatureInput)) // #nosec G401 -- protocol-defined checksum.
	bodySum := md5.Sum(body)                        // #nosec G401 -- protocol-defined checksum.
	machineID := strings.TrimSpace(credential.machineID)
	if machineID == "" {
		machineID = uuid.NewString()
	}

	headers := make(http.Header)
	headers.Set("Authorization", "Bearer COSY."+payloadBase64+"."+hex.EncodeToString(signatureSum[:]))
	headers.Set("Cosy-Key", cosyKey)
	headers.Set("Cosy-User", credential.userID)
	headers.Set("Cosy-Date", timestamp)
	headers.Set("Cosy-Version", qoderIDEVersion)
	headers.Set("Cosy-Machineid", machineID)
	headers.Set("Cosy-Machinetoken", machineID)
	headers.Set("Cosy-Machinetype", qoderMachineType)
	headers.Set("Cosy-Machineos", qoderMachineOS())
	headers.Set("Cosy-Clienttype", qoderClientType)
	headers.Set("Cosy-Clientip", "127.0.0.1")
	headers.Set("Cosy-Bodyhash", hex.EncodeToString(bodySum[:]))
	headers.Set("Cosy-Bodylength", fmt.Sprintf("%d", len(body)))
	headers.Set("Cosy-Sigpath", sigPath)
	headers.Set("Cosy-Data-Policy", qoderDataPolicy)
	headers.Set("Cosy-Organization-Id", "")
	headers.Set("Cosy-Organization-Tags", "")
	headers.Set("Login-Version", qoderLoginVersion)
	headers.Set("X-Request-Id", uuid.NewString())
	return headers, nil
}

func qoderAESKey() ([]byte, error) {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return nil, fmt.Errorf("create Qoder signing key: %w", err)
	}
	key := make([]byte, hex.EncodedLen(len(random)))
	hex.Encode(key, random)
	return key, nil
}

func qoderAESEncrypt(plaintext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create Qoder AES cipher: %w", err)
	}
	padding := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := make([]byte, len(plaintext)+padding)
	copy(padded, plaintext)
	copy(padded[len(plaintext):], bytes.Repeat([]byte{byte(padding)}, padding))
	encrypted := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, key).CryptBlocks(encrypted, padded)
	return encrypted, nil
}

func qoderPublicKey() (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(qoderRSAPublicKeyPEM))
	if block == nil {
		return nil, fmt.Errorf("decode Qoder public key: no PEM block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("decode Qoder public key: %w", err)
	}
	publicKey, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("decode Qoder public key: unexpected key type %T", parsed)
	}
	return publicKey, nil
}

func qoderSignaturePath(requestURL string) (string, error) {
	parsed, err := url.Parse(requestURL)
	if err != nil {
		return "", fmt.Errorf("parse Qoder request URL: %w", err)
	}
	path := parsed.Path
	if strings.HasPrefix(path, "/algo") {
		path = strings.TrimPrefix(path, "/algo")
	}
	return path, nil
}

func qoderMachineOS() string {
	architecture := "x86_64"
	if runtime.GOARCH == "arm64" {
		architecture = "aarch64"
	}
	// Qoder's current COSY protocol groups non-Windows desktop clients under
	// the linux identifier, including macOS. Keep this wire value compatible.
	if runtime.GOOS == "windows" {
		return architecture + "_windows"
	}
	return architecture + "_linux"
}
