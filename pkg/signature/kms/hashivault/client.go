//
// Copyright 2021 The Sigstore Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hashivault

import (
	"context"
	"crypto"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/ReneKroon/ttlcache/v2"
	vault "github.com/hashicorp/vault/api"
	"github.com/mitchellh/go-homedir"
	"github.com/pkg/errors"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"github.com/sigstore/sigstore/pkg/signature"
	sigkms "github.com/sigstore/sigstore/pkg/signature/kms"
)

func init() {
	sigkms.AddProvider(ReferenceScheme, func(_ context.Context, keyResourceID string, hashFunc crypto.Hash, opts ...signature.RPCOption) (sigkms.SignerVerifier, error) {
		return LoadSignerVerifier(keyResourceID, hashFunc, opts...)
	})
}

type hashivaultClient struct {
	client                  *vault.Client
	keyPath                 string
	transitSecretEnginePath string
	keyCache                *ttlcache.Cache
	keyVersion              uint64
}

var (
	errReference   = errors.New("kms specification should be in the format hashivault://<key>")
	referenceRegex = regexp.MustCompile(`^hashivault://(?P<path>\w(([\w-.]+)?\w)?)$`)
	prefixRegex    = regexp.MustCompile("^vault:v[0-9]+:")
)

const (
	vaultV1DataPrefix = "vault:v1:"

	// use a consistent key for cache lookups
	cacheKey = "signer"

	// ReferenceScheme schemes for various KMS services are copied from https://github.com/google/go-cloud/tree/master/secrets
	ReferenceScheme = "hashivault://"
)

// ValidReference returns a non-nil error if the reference string is invalid
func ValidReference(ref string) error {
	if !referenceRegex.MatchString(ref) {
		return errReference
	}
	return nil
}

func parseReference(resourceID string) (keyPath string, err error) {
	i := referenceRegex.SubexpIndex("path")
	v := referenceRegex.FindStringSubmatch(resourceID)
	if len(v) < i+1 {
		err = errors.Errorf("invalid vault format %q", resourceID)
		return
	}
	keyPath = v[i]
	return
}

func newHashivaultClient(address, token, transitSecretEnginePath, keyResourceID string, keyVersion uint64) (*hashivaultClient, error) {
	if err := ValidReference(keyResourceID); err != nil {
		return nil, err
	}

	keyPath, err := parseReference(keyResourceID)
	if err != nil {
		return nil, err
	}

	if address == "" {
		address = os.Getenv("VAULT_ADDR")
	}
	if address == "" {
		return nil, errors.New("VAULT_ADDR is not set")
	}

	client, err := vault.NewClient(&vault.Config{
		Address: address,
	})
	if err != nil {
		return nil, errors.Wrap(err, "new vault client")
	}

	if token == "" {
		token = os.Getenv("VAULT_TOKEN")
	}
	if token == "" {
		log.Printf("VAULT_TOKEN is not set, trying to read token from file at path ~/.vault-token")
		homeDir, err := homedir.Dir()
		if err != nil {
			return nil, errors.Wrap(err, "get home directory")
		}

		tokenFromFile, err := os.ReadFile(filepath.Join(homeDir, ".vault-token"))
		if err != nil {
			return nil, errors.Wrap(err, "read .vault-token file")
		}

		token = string(tokenFromFile)
	}
	client.SetToken(token)

	if transitSecretEnginePath == "" {
		transitSecretEnginePath = os.Getenv("TRANSIT_SECRET_ENGINE_PATH")
	}
	if transitSecretEnginePath == "" {
		transitSecretEnginePath = "transit"
	}

	hvClient := &hashivaultClient{
		client:                  client,
		keyPath:                 keyPath,
		transitSecretEnginePath: transitSecretEnginePath,
		keyCache:                ttlcache.NewCache(),
		keyVersion:              keyVersion,
	}
	hvClient.keyCache.SetLoaderFunction(hvClient.keyCacheLoaderFunction)
	hvClient.keyCache.SkipTTLExtensionOnHit(true)

	return hvClient, nil
}

func oidcLogin(_ context.Context, address, path, role, token string) (string, error) {
	if address == "" {
		address = os.Getenv("VAULT_ADDR")
	}
	if address == "" {
		return "", errors.New("VAULT_ADDR is not set")
	}
	if path == "" {
		path = "jwt"
	}

	client, err := vault.NewClient(&vault.Config{
		Address: address,
	})
	if err != nil {
		return "", errors.Wrap(err, "new vault client")
	}

	loginData := map[string]interface{}{
		"role": role,
		"jwt":  token,
	}
	fullpath := fmt.Sprintf("auth/%s/login", path)
	resp, err := client.Logical().Write(fullpath, loginData)
	if err != nil {
		return "", errors.Wrap(err, "vault oidc login")
	}
	return resp.TokenID()
}

func (h *hashivaultClient) keyCacheLoaderFunction(key string) (data interface{}, ttl time.Duration, err error) {
	ttl = time.Second * 300
	var pubKey crypto.PublicKey
	pubKey, err = h.fetchPublicKey(context.Background())
	if err != nil {
		data = nil
		return
	}
	data = pubKey
	return data, ttl, err
}

func (h *hashivaultClient) fetchPublicKey(_ context.Context) (crypto.PublicKey, error) {
	client := h.client.Logical()

	keyResult, err := client.Read(fmt.Sprintf("/%s/keys/%s", h.transitSecretEnginePath, h.keyPath))
	if err != nil {
		return nil, errors.Wrap(err, "public key")
	}

	keysData, hasKeys := keyResult.Data["keys"]
	latestVersion, hasVersion := keyResult.Data["latest_version"]
	if !hasKeys || !hasVersion {
		return nil, errors.New("Failed to read transit key keys: corrupted response")
	}

	keys, ok := keysData.(map[string]interface{})
	if !ok {
		return nil, errors.New("Failed to read transit key keys: Invalid keys map")
	}

	keyVersion := latestVersion.(json.Number)
	keyData, ok := keys[string(keyVersion)]
	if !ok {
		return nil, errors.New("Failed to read transit key keys: corrupted response")
	}

	publicKeyPem, ok := keyData.(map[string]interface{})["public_key"]
	if !ok {
		return nil, errors.New("Failed to read transit key keys: corrupted response")
	}

	return cryptoutils.UnmarshalPEMToPublicKey([]byte(publicKeyPem.(string)))
}

func (h *hashivaultClient) public() (crypto.PublicKey, error) {
	return h.keyCache.Get(cacheKey)
}

func (h hashivaultClient) sign(digest []byte, alg crypto.Hash, opts ...signature.SignOption) ([]byte, error) {
	client := h.client.Logical()

	keyVersion := fmt.Sprintf("%d", h.keyVersion)
	var keyVersionUsedPtr *string
	for _, opt := range opts {
		opt.ApplyKeyVersion(&keyVersion)
		opt.ApplyKeyVersionUsed(&keyVersionUsedPtr)
	}

	if keyVersion != "" {
		if _, err := strconv.ParseUint(keyVersion, 10, 64); err != nil {
			return nil, errors.Wrap(err, "parsing requested key version")
		}
	}

	signResult, err := client.Write(fmt.Sprintf("/%s/sign/%s%s", h.transitSecretEnginePath, h.keyPath, hashString(alg)), map[string]interface{}{
		"input":       base64.StdEncoding.Strict().EncodeToString(digest),
		"prehashed":   alg != crypto.Hash(0),
		"key_version": keyVersion,
	})
	if err != nil {
		return nil, errors.Wrap(err, "Transit: failed to sign payload")
	}

	encodedSignature, ok := signResult.Data["signature"]
	if !ok {
		return nil, errors.New("Transit: response corrupted in-transit")
	}

	return vaultDecode(encodedSignature, keyVersionUsedPtr)
}

func (h hashivaultClient) verify(sig, digest []byte, alg crypto.Hash, opts ...signature.VerifyOption) error {
	client := h.client.Logical()
	encodedSig := base64.StdEncoding.EncodeToString(sig)

	keyVersion := ""
	for _, opt := range opts {
		opt.ApplyKeyVersion(&keyVersion)
	}

	var vaultDataPrefix string
	if keyVersion != "" {
		// keyVersion >= 1 on verification but can be set to 0 on signing
		kvUint, err := strconv.ParseUint(keyVersion, 10, 64)
		if err != nil {
			return errors.Wrap(err, "parsing requested key version")
		} else if kvUint == 0 {
			return errors.New("key version must be >= 1")
		}

		vaultDataPrefix = fmt.Sprintf("vault:v%d:", kvUint)
	} else {
		vaultDataPrefix = os.Getenv("VAULT_KEY_PREFIX")
		if vaultDataPrefix == "" {
			if h.keyVersion > 0 {
				vaultDataPrefix = fmt.Sprintf("vault:v%d:", h.keyVersion)
			} else {
				vaultDataPrefix = vaultV1DataPrefix
			}
		}
	}

	result, err := client.Write(fmt.Sprintf("/%s/verify/%s/%s", h.transitSecretEnginePath, h.keyPath, hashString(alg)), map[string]interface{}{
		"input":     base64.StdEncoding.EncodeToString(digest),
		"prehashed": alg != crypto.Hash(0),
		"signature": fmt.Sprintf("%s%s", vaultDataPrefix, encodedSig),
	})

	if err != nil {
		return errors.Wrap(err, "verify")
	}

	valid, ok := result.Data["valid"]
	if !ok {
		return errors.New("corrupted response")
	}

	isValid, ok := valid.(bool)
	if !ok {
		return fmt.Errorf("data type assertion for field `valid` failed: %T %#v", valid.(bool), valid.(bool))
	}

	if !isValid {
		return errors.New("Failed vault verification")
	}

	return nil
}

// Vault likes to prefix base64 data with a version prefix
func vaultDecode(data interface{}, keyVersionUsed *string) ([]byte, error) {
	encoded, ok := data.(string)
	if !ok {
		return nil, errors.New("Received non-string data")
	}

	if keyVersionUsed != nil {
		*keyVersionUsed = prefixRegex.FindString(encoded)
	}
	return base64.StdEncoding.DecodeString(prefixRegex.ReplaceAllString(encoded, ""))
}

func hashString(h crypto.Hash) string {
	var hashStr string
	switch h {
	case crypto.SHA224:
		hashStr = "/sha2-224"
	case crypto.SHA256:
		hashStr = "/sha2-256"
	case crypto.SHA384:
		hashStr = "/sha2-384"
	case crypto.SHA512:
		hashStr = "/sha2-512"
	default:
		hashStr = ""
	}
	return hashStr
}

func (h hashivaultClient) createKey(typeStr string) (crypto.PublicKey, error) {
	client := h.client.Logical()

	if _, err := client.Write(fmt.Sprintf("/%s/keys/%s", h.transitSecretEnginePath, h.keyPath), map[string]interface{}{
		"type": typeStr,
	}); err != nil {
		return nil, errors.Wrap(err, "Failed to create transit key")
	}
	return h.public()
}
