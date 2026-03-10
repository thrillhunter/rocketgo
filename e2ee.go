package rocket

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/pbkdf2"
)

const (
	legacyEncryptedMessageKeyIDLen = 12
	prefixedBase64PayloadBytes     = 256
	prefixedBase64EncodedBytes     = 344
)

type e2eeIdentity struct {
	PublicKey  string `json:"public_key"`
	PrivateKey string `json:"private_key"`
}

type e2eeRoomState struct {
	RoomID     string
	KeyID      string
	SessionKey []byte
	SessionJWK string
	OldKeys    map[string][]byte
}

type uploadPayload struct {
	FileName             string
	ContentType          string
	UploadBytes          []byte
	EncryptedFileContent *EncryptedContent
	MessageType          string
	EncryptedFile        *preparedFile
	PlainFile            *File
}

type preparedFile struct {
	OriginalName string
	ContentType  string
	Size         int64
	Description  string
	KeyJWK       map[string]any
	IV           string
	Hash         string
}

type e2eeManager struct {
	client *Client

	mu         sync.RWMutex
	publicKey  *rsa.PublicKey
	privateKey *rsa.PrivateKey
	publicJWK  string
	identity   e2eeIdentity
	rooms      map[string]*e2eeRoomState
}

func newE2EEManager(client *Client) *e2eeManager {
	return &e2eeManager{
		client: client,
		rooms:  make(map[string]*e2eeRoomState),
	}
}

func (m *e2eeManager) init(ctx context.Context) error {
	if !m.client.cfg.E2EE.Enabled {
		return nil
	}
	if strings.TrimSpace(m.client.cfg.E2EE.Password) == "" {
		return errors.New("rocket: e2ee password is required when e2ee is enabled")
	}

	identity, err := m.fetchIdentity(ctx)
	if err != nil {
		if m.identity.PublicKey == "" || m.identity.PrivateKey == "" {
			return err
		}
		identity = m.identity
	}

	if identity.PublicKey == "" || identity.PrivateKey == "" {
		if m.identity.PublicKey != "" && m.identity.PrivateKey != "" {
			identity = m.identity
			if err := m.persistIdentityToServer(ctx, identity.PublicKey, identity.PrivateKey); err != nil {
				return err
			}
		} else {
			identity, err = m.generateIdentity(ctx)
			if err != nil {
				return err
			}
		}
	}

	privateJWK, err := decryptStoredPrivateKey(m.client.Session().UserID, identity.PrivateKey, m.client.cfg.E2EE.Password)
	if err != nil {
		return fmt.Errorf("rocket: decrypt private e2ee key: %w", err)
	}
	priv, err := importRSAPrivateJWK(privateJWK)
	if err != nil {
		return err
	}
	pub, err := importRSAPublicJWK(identity.PublicKey)
	if err != nil {
		return err
	}

	m.mu.Lock()
	m.identity = identity
	m.publicJWK = identity.PublicKey
	m.publicKey = pub
	m.privateKey = priv
	m.mu.Unlock()
	return m.saveIdentityToDisk()
}

func (m *e2eeManager) loadIdentityFromDisk() error {
	paths := m.identityPaths()
	if len(paths) == 0 {
		return nil
	}

	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}

		var identity e2eeIdentity
		if err := json.Unmarshal(data, &identity); err != nil {
			return err
		}
		if identity.PublicKey != "" && identity.PrivateKey != "" {
			m.identity = identity
			if canonical := m.identityPath(); canonical != "" && path != canonical {
				_ = m.saveIdentityToDisk()
				_ = os.Remove(path)
			}
			return nil
		}
	}
	return nil
}

func (m *e2eeManager) saveIdentityToDisk() error {
	path := m.identityPath()
	if path == "" || m.identity.PublicKey == "" || m.identity.PrivateKey == "" {
		return nil
	}

	data, err := json.MarshalIndent(m.identity, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func (m *e2eeManager) identityPath() string {
	return m.client.accountPersistencePath("identity.json")
}

func (m *e2eeManager) identityPaths() []string {
	return m.client.persistencePathCandidates("identity.json")
}

func (m *e2eeManager) fetchIdentity(ctx context.Context) (e2eeIdentity, error) {
	var resp struct {
		Success    bool   `json:"success"`
		PublicKey  string `json:"public_key"`
		PrivateKey string `json:"private_key"`
	}
	if err := m.client.doJSON(ctx, http.MethodGet, "/e2e.fetchMyKeys", nil, nil, &resp); err != nil {
		return e2eeIdentity{}, err
	}

	identity := e2eeIdentity{
		PublicKey:  resp.PublicKey,
		PrivateKey: resp.PrivateKey,
	}
	if identity.PublicKey != "" && identity.PrivateKey != "" {
		m.identity = identity
		_ = m.saveIdentityToDisk()
	}
	return identity, nil
}

func (m *e2eeManager) generateIdentity(ctx context.Context) (e2eeIdentity, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return e2eeIdentity{}, err
	}

	publicJWK, err := exportRSAPublicJWK(&key.PublicKey)
	if err != nil {
		return e2eeIdentity{}, err
	}
	privateJWK, err := exportRSAPrivateJWK(key)
	if err != nil {
		return e2eeIdentity{}, err
	}
	encryptedPrivate, err := encryptStoredPrivateKey(m.client.Session().UserID, privateJWK, m.client.cfg.E2EE.Password)
	if err != nil {
		return e2eeIdentity{}, err
	}

	identity := e2eeIdentity{
		PublicKey:  publicJWK,
		PrivateKey: encryptedPrivate,
	}
	if err := m.persistIdentityToServer(ctx, identity.PublicKey, identity.PrivateKey); err != nil {
		return e2eeIdentity{}, err
	}
	m.identity = identity
	_ = m.saveIdentityToDisk()
	return identity, nil
}

func (m *e2eeManager) persistIdentityToServer(ctx context.Context, publicKey, privateKey string) error {
	return m.client.doJSON(ctx, http.MethodPost, "/e2e.setUserPublicAndPrivateKeys", nil, map[string]any{
		"public_key":  publicKey,
		"private_key": privateKey,
	}, nil)
}

func (m *e2eeManager) requestSubscriptionKeys(ctx context.Context) error {
	_, err := m.client.ddp.call(ctx, "e2e.requestSubscriptionKeys")
	return err
}

func (m *e2eeManager) primeSubscriptions(ctx context.Context) error {
	if !m.client.cfg.E2EE.Enabled {
		return nil
	}

	subs := m.client.Subscriptions()
	var firstErr error
	for i := range subs {
		sub := subs[i]
		if sub.RoomID == "" {
			continue
		}
		if !sub.Encrypted && sub.E2EKey == "" && sub.E2ESuggestedKey == "" && len(sub.OldRoomKeys) == 0 {
			continue
		}
		room, err := m.client.getOrFetchRoom(ctx, sub.RoomID)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := m.applySubscription(ctx, room, &sub); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *e2eeManager) handleRoomUpdate(room *Room) {
	if room == nil || room.ID == "" || room.E2EKeyID == "" {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	state, ok := m.rooms[room.ID]
	if !ok {
		return
	}
	if state.KeyID == "" {
		state.KeyID = room.E2EKeyID
		return
	}
	if state.KeyID != room.E2EKeyID {
		if state.OldKeys == nil {
			state.OldKeys = make(map[string][]byte)
		}
		if len(state.SessionKey) > 0 {
			state.OldKeys[state.KeyID] = append([]byte(nil), state.SessionKey...)
		}
		state.KeyID = room.E2EKeyID
		state.SessionKey = nil
		state.SessionJWK = ""
	}
}

func (m *e2eeManager) handleSubscriptionUpdate(ctx context.Context, sub *Subscription) error {
	if sub == nil || sub.RoomID == "" {
		return nil
	}

	room, err := m.client.getOrFetchRoom(ctx, sub.RoomID)
	if err != nil {
		return err
	}
	return m.applySubscription(ctx, room, sub)
}

func (m *e2eeManager) handleKeyRequest(ctx context.Context, roomID, keyID string) error {
	state := m.getRoomState(roomID)
	if state == nil || state.KeyID != keyID || len(state.SessionKey) == 0 {
		return nil
	}
	return m.distributeRoomKey(ctx, roomID)
}

func (m *e2eeManager) encryptTextMessage(ctx context.Context, room *Room, text string) (map[string]any, error) {
	state, err := m.ensureRoom(ctx, room.ID)
	if err != nil {
		return nil, err
	}
	content, err := m.encryptMessageContent(state, map[string]any{"msg": text})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"t":       "e2e",
		"e2e":     "pending",
		"content": content,
	}, nil
}

func (m *e2eeManager) decryptMessage(ctx context.Context, message *Message) error {
	if message == nil || message.Type != "e2e" {
		return nil
	}

	state, err := m.ensureRoom(ctx, message.RoomID)
	if err != nil {
		return err
	}
	payload, err := decodeEncryptedPayload(message)
	if err != nil {
		return err
	}

	keys := m.keysForPayload(state, payload.KeyID)
	if len(keys) == 0 {
		return errors.New("rocket: no e2ee room key available")
	}

	var (
		plaintext []byte
		lastErr   error
	)
	for _, key := range keys {
		plaintext, err = decryptAESPayload(key, payload)
		if err == nil {
			break
		}
		lastErr = err
	}
	if plaintext == nil {
		return lastErr
	}

	var body struct {
		Msg         string       `json:"msg"`
		Text        string       `json:"text"`
		Attachments []Attachment `json:"attachments"`
		Files       []File       `json:"files"`
		File        *File        `json:"file"`
	}
	if err := json.Unmarshal(plaintext, &body); err != nil {
		return err
	}

	if body.Msg != "" {
		message.Text = body.Msg
	} else if body.Text != "" {
		message.Text = body.Text
	}
	if len(body.Attachments) > 0 {
		message.Attachments = body.Attachments
	}
	if len(body.Files) > 0 {
		message.Files = body.Files
	}
	if body.File != nil {
		message.File = body.File
	}
	message.Encrypted = true
	message.Decrypted = true
	return nil
}

func (m *e2eeManager) prepareUpload(ctx context.Context, room *Room, fileName, contentType string, data []byte, description string) (*uploadPayload, error) {
	state, err := m.ensureRoom(ctx, room.ID)
	if err != nil {
		return nil, err
	}

	keyJWK, iv, encryptedBytes, hash, hashedName, err := encryptFileBytes(fileName, data)
	if err != nil {
		return nil, err
	}

	typeGroup := contentType
	if parts := strings.SplitN(contentType, "/", 2); len(parts) == 2 {
		typeGroup = parts[0]
	}

	fileContent, err := m.encryptMessageContent(state, map[string]any{
		"type":      contentType,
		"typeGroup": typeGroup,
		"name":      fileName,
		"encryption": map[string]any{
			"key": keyJWK,
			"iv":  iv,
		},
		"hashes": map[string]any{
			"sha256": hash,
		},
	})
	if err != nil {
		return nil, err
	}

	return &uploadPayload{
		FileName:             hashedName,
		ContentType:          contentType,
		UploadBytes:          encryptedBytes,
		EncryptedFileContent: fileContent,
		MessageType:          "e2e",
		EncryptedFile: &preparedFile{
			OriginalName: fileName,
			ContentType:  contentType,
			Size:         int64(len(data)),
			Description:  description,
			KeyJWK:       keyJWK,
			IV:           iv,
			Hash:         hash,
		},
	}, nil
}

func (m *e2eeManager) buildEncryptedUploadMessage(ctx context.Context, room *Room, fileID, fileURL string, prepared *preparedFile) (*EncryptedContent, *File, error) {
	state, err := m.ensureRoom(ctx, room.ID)
	if err != nil {
		return nil, nil, err
	}

	attachment := Attachment{
		Title:             prepared.OriginalName,
		Type:              "file",
		Description:       prepared.Description,
		TitleLink:         fileURL,
		TitleLinkDownload: true,
		FileID:            fileID,
		Encryption: &FileEncryption{
			Key: prepared.KeyJWK,
			IV:  prepared.IV,
		},
		Hashes: &FileHashes{SHA256: prepared.Hash},
	}

	switch {
	case strings.HasPrefix(prepared.ContentType, "image/"):
		attachment.ImageURL = fileURL
		attachment.ImageType = prepared.ContentType
		attachment.ImageSize = prepared.Size
	case strings.HasPrefix(prepared.ContentType, "audio/"):
		attachment.AudioURL = fileURL
		attachment.AudioType = prepared.ContentType
		attachment.AudioSize = prepared.Size
	case strings.HasPrefix(prepared.ContentType, "video/"):
		attachment.VideoURL = fileURL
		attachment.VideoType = prepared.ContentType
		attachment.VideoSize = prepared.Size
	default:
		attachment.Size = prepared.Size
		attachment.Format = fileExtension(prepared.OriginalName)
	}

	typeGroup := prepared.ContentType
	if parts := strings.SplitN(prepared.ContentType, "/", 2); len(parts) == 2 {
		typeGroup = parts[0]
	}
	fileRef := File{
		ID:        fileID,
		Name:      prepared.OriginalName,
		Type:      prepared.ContentType,
		Size:      prepared.Size,
		TypeGroup: typeGroup,
		URL:       fileURL,
	}

	content, err := m.encryptMessageContent(state, map[string]any{
		"attachments": []Attachment{attachment},
		"files":       []File{fileRef},
		"file":        fileRef,
	})
	if err != nil {
		return nil, nil, err
	}
	return content, &fileRef, nil
}

func (m *e2eeManager) decryptAttachmentData(attachment Attachment, data []byte) ([]byte, error) {
	if attachment.Encryption == nil {
		return data, nil
	}

	keyBytes, err := importOctJWKMap(attachment.Encryption.Key)
	if err != nil {
		return nil, err
	}
	iv, err := base64.StdEncoding.DecodeString(attachment.Encryption.IV)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(keyBytes)
	if err != nil {
		return nil, err
	}
	stream := cipher.NewCTR(block, iv)
	out := make([]byte, len(data))
	stream.XORKeyStream(out, data)

	if attachment.Hashes != nil && attachment.Hashes.SHA256 != "" {
		sum := sha256.Sum256(out)
		if fmt.Sprintf("%x", sum[:]) != attachment.Hashes.SHA256 {
			return nil, errors.New("rocket: decrypted file hash mismatch")
		}
	}
	return out, nil
}

func (m *e2eeManager) ensureRoom(ctx context.Context, roomID string) (*e2eeRoomState, error) {
	room, err := m.client.getOrFetchRoom(ctx, roomID)
	if err != nil {
		return nil, err
	}
	if !room.Encrypted {
		return nil, errors.New("rocket: room is not encrypted")
	}

	state := m.getOrCreateRoomState(roomID)
	if room.E2EKeyID != "" && state.KeyID != "" && room.E2EKeyID != state.KeyID {
		m.resetRoomState(roomID, room.E2EKeyID)
		state = m.getOrCreateRoomState(roomID)
	}

	if sub, ok := m.client.getSubscriptionByRoomID(roomID); ok {
		if err := m.applySubscription(ctx, room, sub); err != nil {
			return nil, err
		}
		state = m.getOrCreateRoomState(roomID)
		if len(state.SessionKey) > 0 {
			return state, nil
		}
	}

	if room.E2EKeyID == "" {
		return m.createRoomKey(ctx, roomID)
	}

	if err := m.requestSubscriptionKeys(ctx); err != nil {
		m.client.log.Debug("request subscription keys failed", "room_id", roomID, "error", err)
	}

	refreshTicker := time.NewTicker(time.Second)
	defer refreshTicker.Stop()
	timeout := time.NewTimer(15 * time.Second)
	defer timeout.Stop()

	for {
		if sub, ok := m.client.getSubscriptionByRoomID(roomID); ok {
			if err := m.applySubscription(ctx, room, sub); err != nil {
				return nil, err
			}
			state = m.getOrCreateRoomState(roomID)
			if len(state.SessionKey) > 0 {
				return state, nil
			}
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout.C:
			return nil, fmt.Errorf("rocket: timed out waiting for e2ee key for room %s", roomID)
		case <-refreshTicker.C:
			if _, err := m.client.GetSubscriptions(ctx); err != nil {
				m.client.log.Debug("subscription refresh failed", "room_id", roomID, "error", err)
			}
		}
	}
}

func (m *e2eeManager) applySubscription(ctx context.Context, room *Room, sub *Subscription) error {
	state := m.getOrCreateRoomState(sub.RoomID)
	if len(sub.OldRoomKeys) > 0 {
		if err := m.loadOldKeys(state, sub.OldRoomKeys); err != nil {
			return err
		}
	}

	if sub.E2ESuggestedKey != "" && sub.E2EKey == "" {
		ok, err := m.importGroupKey(sub.RoomID, sub.E2ESuggestedKey)
		if err != nil || !ok {
			_ = m.client.doJSON(ctx, http.MethodPost, "/e2e.rejectSuggestedGroupKey", nil, map[string]string{"rid": sub.RoomID}, nil)
			return err
		}
		if err := m.client.doJSON(ctx, http.MethodPost, "/e2e.acceptSuggestedGroupKey", nil, map[string]string{"rid": sub.RoomID}, nil); err != nil {
			return err
		}
		sub.E2EKey = sub.E2ESuggestedKey
		sub.E2ESuggestedKey = ""
		m.client.upsertSubscription(sub)
	}

	if sub.E2EKey != "" {
		_, err := m.importGroupKey(sub.RoomID, sub.E2EKey)
		return err
	}

	if room != nil && room.E2EKeyID != "" && state.KeyID == "" {
		m.resetRoomState(sub.RoomID, room.E2EKeyID)
	}
	return nil
}

func (m *e2eeManager) createRoomKey(ctx context.Context, roomID string) (*e2eeRoomState, error) {
	_, publicJWK, err := m.publicIdentity()
	if err != nil {
		return nil, err
	}

	keyBytes, sessionJWK, err := generateSessionKeyJWK("A256GCM")
	if err != nil {
		return nil, err
	}
	keyID := uuid.NewString()
	if err := m.client.doJSON(ctx, http.MethodPost, "/e2e.setRoomKeyID", nil, map[string]string{
		"rid":   roomID,
		"keyID": keyID,
	}, nil); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			if room, roomErr := m.client.GetRoomInfo(ctx, roomID); roomErr == nil {
				m.handleRoomUpdate(room)
				return m.ensureRoom(ctx, roomID)
			}
		}
		return nil, err
	}

	encrypted, err := m.encryptGroupKeyForParticipant(publicJWK, keyID, sessionJWK)
	if err != nil {
		return nil, err
	}
	if err := m.client.doJSON(ctx, http.MethodPost, "/e2e.updateGroupKey", nil, map[string]string{
		"rid": roomID,
		"uid": m.client.Session().UserID,
		"key": encrypted,
	}, nil); err != nil {
		return nil, err
	}

	state := &e2eeRoomState{
		RoomID:     roomID,
		KeyID:      keyID,
		SessionKey: keyBytes,
		SessionJWK: sessionJWK,
		OldKeys:    make(map[string][]byte),
	}
	m.storeRoomState(state)
	if err := m.distributeRoomKey(ctx, roomID); err != nil {
		m.client.log.Debug("room key distribution failed", "room_id", roomID, "error", err)
	}
	return state, nil
}

func (m *e2eeManager) distributeRoomKey(ctx context.Context, roomID string) error {
	state := m.getRoomState(roomID)
	if state == nil || state.SessionJWK == "" {
		return nil
	}

	query := url.Values{}
	query.Set("rid", roomID)
	var resp struct {
		Success bool `json:"success"`
		Users   []struct {
			ID  string `json:"_id"`
			E2E struct {
				PublicKey string `json:"public_key"`
			} `json:"e2e"`
		} `json:"users"`
	}
	if err := m.client.doJSON(ctx, http.MethodGet, "/e2e.getUsersOfRoomWithoutKey", query, nil, &resp); err != nil {
		return err
	}

	users := make([]map[string]any, 0, len(resp.Users))
	for _, user := range resp.Users {
		if user.E2E.PublicKey == "" {
			continue
		}
		encrypted, err := m.encryptGroupKeyForParticipant(user.E2E.PublicKey, state.KeyID, state.SessionJWK)
		if err != nil {
			return err
		}
		entry := map[string]any{
			"_id": user.ID,
			"key": encrypted,
		}
		if len(state.OldKeys) > 0 {
			oldKeys, err := m.encryptOldKeysForParticipant(user.E2E.PublicKey, state)
			if err != nil {
				return err
			}
			if len(oldKeys) > 0 {
				entry["oldKeys"] = oldKeys
			}
		}
		users = append(users, entry)
	}
	if len(users) == 0 {
		return nil
	}

	return m.client.doJSON(ctx, http.MethodPost, "/e2e.provideUsersSuggestedGroupKeys", nil, map[string]any{
		"usersSuggestedGroupKeys": map[string]any{
			roomID: users,
		},
	}, nil)
}

func (m *e2eeManager) encryptOldKeysForParticipant(publicJWK string, state *e2eeRoomState) ([]map[string]any, error) {
	result := make([]map[string]any, 0, len(state.OldKeys))
	for keyID, keyBytes := range state.OldKeys {
		sessionJWK, err := exportOctJWK(keyBytes, "A256GCM")
		if err != nil {
			return nil, err
		}
		encrypted, err := m.encryptGroupKeyForParticipant(publicJWK, keyID, sessionJWK)
		if err != nil {
			return nil, err
		}
		result = append(result, map[string]any{
			"E2EKey":   encrypted,
			"e2eKeyId": keyID,
			"ts":       time.Now().UTC().Format(time.RFC3339Nano),
		})
	}
	return result, nil
}

func (m *e2eeManager) importGroupKey(roomID, encoded string) (bool, error) {
	privateKey, err := m.privateIdentity()
	if err != nil {
		return false, err
	}

	keyID, ciphertext, err := decodePrefixedBase64(encoded)
	if err != nil {
		return false, err
	}
	plaintext, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, privateKey, ciphertext, nil)
	if err != nil {
		return false, err
	}
	keyBytes, err := importOctJWK(string(plaintext))
	if err != nil {
		return false, err
	}

	state := m.getOrCreateRoomState(roomID)
	state.KeyID = keyID
	state.SessionKey = keyBytes
	state.SessionJWK = string(plaintext)
	if state.OldKeys == nil {
		state.OldKeys = make(map[string][]byte)
	}
	m.storeRoomState(state)
	return true, nil
}

func (m *e2eeManager) loadOldKeys(state *e2eeRoomState, oldKeys []RoomKey) error {
	privateKey, err := m.privateIdentity()
	if err != nil {
		return err
	}
	if state.OldKeys == nil {
		state.OldKeys = make(map[string][]byte)
	}
	for _, old := range oldKeys {
		if old.E2EKey == "" || old.E2EKeyID == "" {
			continue
		}
		_, ciphertext, err := decodePrefixedBase64(old.E2EKey)
		if err != nil {
			return err
		}
		plaintext, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, privateKey, ciphertext, nil)
		if err != nil {
			return err
		}
		keyBytes, err := importOctJWK(string(plaintext))
		if err != nil {
			return err
		}
		state.OldKeys[old.E2EKeyID] = keyBytes
	}
	m.storeRoomState(state)
	return nil
}

func (m *e2eeManager) encryptGroupKeyForParticipant(publicJWK, keyID, sessionJWK string) (string, error) {
	publicKey, err := importRSAPublicJWK(publicJWK)
	if err != nil {
		return "", err
	}
	ciphertext, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, publicKey, []byte(sessionJWK), nil)
	if err != nil {
		return "", err
	}
	return encodePrefixedBase64(keyID, ciphertext), nil
}

func (m *e2eeManager) encryptMessageContent(state *e2eeRoomState, payload any) (*EncryptedContent, error) {
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	iv := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(state.SessionKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ciphertext := gcm.Seal(nil, iv, plaintext, nil)
	return &EncryptedContent{
		Algorithm:  "rc.v2.aes-sha2",
		KeyID:      state.KeyID,
		IV:         base64.StdEncoding.EncodeToString(iv),
		Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
	}, nil
}

func (m *e2eeManager) keysForPayload(state *e2eeRoomState, keyID string) [][]byte {
	if state == nil {
		return nil
	}

	keys := make([][]byte, 0, len(state.OldKeys)+1)
	seen := make(map[string]struct{}, len(state.OldKeys)+1)
	add := func(key []byte) {
		if len(key) == 0 {
			return
		}
		fingerprint := string(key)
		if _, ok := seen[fingerprint]; ok {
			return
		}
		seen[fingerprint] = struct{}{}
		keys = append(keys, append([]byte(nil), key...))
	}

	if keyID != "" {
		if old, ok := state.OldKeys[keyID]; ok {
			add(old)
		}
		if state.KeyID == keyID {
			add(state.SessionKey)
		}
	}
	add(state.SessionKey)

	oldKeyIDs := make([]string, 0, len(state.OldKeys))
	for id := range state.OldKeys {
		oldKeyIDs = append(oldKeyIDs, id)
	}
	sort.Strings(oldKeyIDs)
	for _, id := range oldKeyIDs {
		add(state.OldKeys[id])
	}
	return keys
}

func (m *e2eeManager) getRoomState(roomID string) *e2eeRoomState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	state, ok := m.rooms[roomID]
	if !ok {
		return nil
	}
	return cloneRoomState(state)
}

func (m *e2eeManager) getOrCreateRoomState(roomID string) *e2eeRoomState {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.rooms[roomID]
	if !ok {
		state = &e2eeRoomState{
			RoomID:  roomID,
			OldKeys: make(map[string][]byte),
		}
		m.rooms[roomID] = state
	}
	return cloneRoomState(state)
}

func (m *e2eeManager) storeRoomState(state *e2eeRoomState) {
	if state == nil {
		return
	}
	m.mu.Lock()
	m.rooms[state.RoomID] = cloneRoomState(state)
	m.mu.Unlock()
}

func (m *e2eeManager) resetRoomState(roomID, keyID string) {
	m.mu.Lock()
	m.rooms[roomID] = &e2eeRoomState{
		RoomID:  roomID,
		KeyID:   keyID,
		OldKeys: make(map[string][]byte),
	}
	m.mu.Unlock()
}

func (m *e2eeManager) removeRoomState(roomID string) {
	if roomID == "" {
		return
	}
	m.mu.Lock()
	delete(m.rooms, roomID)
	m.mu.Unlock()
}

func (m *e2eeManager) publicIdentity() (*rsa.PublicKey, string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.publicKey == nil || m.publicJWK == "" {
		return nil, "", errors.New("rocket: e2ee public key not initialized")
	}
	return m.publicKey, m.publicJWK, nil
}

func (m *e2eeManager) privateIdentity() (*rsa.PrivateKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.privateKey == nil {
		return nil, errors.New("rocket: e2ee private key not initialized")
	}
	return m.privateKey, nil
}

func decodeEncryptedPayload(message *Message) (*EncryptedContent, error) {
	if message == nil {
		return nil, errors.New("rocket: nil message")
	}
	if message.Content != nil && message.Content.Ciphertext != "" {
		return message.Content, nil
	}
	if len(message.Text) < legacyEncryptedMessageKeyIDLen {
		return nil, errors.New("rocket: invalid legacy encrypted payload")
	}
	decoded, err := base64.StdEncoding.DecodeString(message.Text[legacyEncryptedMessageKeyIDLen:])
	if err != nil {
		return nil, err
	}
	if len(decoded) < 16 {
		return nil, errors.New("rocket: invalid legacy encrypted payload")
	}
	return &EncryptedContent{
		Algorithm:  "rc.v1.aes-sha2",
		KeyID:      message.Text[:legacyEncryptedMessageKeyIDLen],
		IV:         base64.StdEncoding.EncodeToString(decoded[:16]),
		Ciphertext: base64.StdEncoding.EncodeToString(decoded[16:]),
	}, nil
}

func decryptAESPayload(key []byte, payload *EncryptedContent) ([]byte, error) {
	iv, err := base64.StdEncoding.DecodeString(payload.IV)
	if err != nil {
		return nil, err
	}
	ciphertext, err := base64.StdEncoding.DecodeString(payload.Ciphertext)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(iv) == 16 {
		if len(ciphertext)%aes.BlockSize != 0 {
			return nil, errors.New("rocket: invalid legacy ciphertext size")
		}
		plaintext := make([]byte, len(ciphertext))
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(plaintext, ciphertext)
		return pkcs7Unpad(plaintext)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, iv, ciphertext, nil)
}

func pkcs7Unpad(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("rocket: invalid padding")
	}
	padding := int(data[len(data)-1])
	if padding == 0 || padding > len(data) {
		return nil, errors.New("rocket: invalid padding")
	}
	for _, b := range data[len(data)-padding:] {
		if int(b) != padding {
			return nil, errors.New("rocket: invalid padding")
		}
	}
	return data[:len(data)-padding], nil
}

func encryptStoredPrivateKey(userID, privateJWK, password string) (string, error) {
	salt := fmt.Sprintf("v2:%s:%s", userID, uuid.NewString())
	key := pbkdf2.Key([]byte(password), []byte(salt), 100000, 32, sha256.New)
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	iv := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return "", err
	}
	ciphertext := gcm.Seal(nil, iv, []byte(privateJWK), nil)
	data, err := json.Marshal(map[string]any{
		"iv":         base64.StdEncoding.EncodeToString(iv),
		"ciphertext": base64.StdEncoding.EncodeToString(ciphertext),
		"salt":       salt,
		"iterations": 100000,
	})
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func decryptStoredPrivateKey(userID, encrypted, password string) (string, error) {
	var v2 struct {
		IV         string `json:"iv"`
		Ciphertext string `json:"ciphertext"`
		Salt       string `json:"salt"`
		Iterations int    `json:"iterations"`
	}
	if err := json.Unmarshal([]byte(encrypted), &v2); err == nil && v2.IV != "" && v2.Ciphertext != "" {
		key := pbkdf2.Key([]byte(password), []byte(v2.Salt), v2.Iterations, 32, sha256.New)
		block, err := aes.NewCipher(key)
		if err != nil {
			return "", err
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return "", err
		}
		iv, err := base64.StdEncoding.DecodeString(v2.IV)
		if err != nil {
			return "", err
		}
		ciphertext, err := base64.StdEncoding.DecodeString(v2.Ciphertext)
		if err != nil {
			return "", err
		}
		plaintext, err := gcm.Open(nil, iv, ciphertext, nil)
		if err != nil {
			return "", err
		}
		return string(plaintext), nil
	}

	var legacy struct {
		Binary string `json:"$binary"`
	}
	if err := json.Unmarshal([]byte(encrypted), &legacy); err == nil && legacy.Binary != "" {
		decoded, err := base64.StdEncoding.DecodeString(legacy.Binary)
		if err != nil {
			return "", err
		}
		if len(decoded) < 16 {
			return "", errors.New("rocket: invalid legacy encrypted private key")
		}
		key := pbkdf2.Key([]byte(password), []byte(userID), 1000, 32, sha256.New)
		block, err := aes.NewCipher(key)
		if err != nil {
			return "", err
		}
		ciphertext := decoded[16:]
		if len(ciphertext)%aes.BlockSize != 0 {
			return "", errors.New("rocket: invalid legacy private key ciphertext")
		}
		plaintext := make([]byte, len(ciphertext))
		cipher.NewCBCDecrypter(block, decoded[:16]).CryptBlocks(plaintext, ciphertext)
		plaintext, err = pkcs7Unpad(plaintext)
		if err != nil {
			return "", err
		}
		return string(plaintext), nil
	}

	return "", errors.New("rocket: unsupported encrypted private key format")
}

func generateSessionKeyJWK(alg string) ([]byte, string, error) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, "", err
	}
	jwk, err := exportOctJWK(key, alg)
	if err != nil {
		return nil, "", err
	}
	return key, jwk, nil
}

func exportOctJWK(key []byte, alg string) (string, error) {
	data, err := json.Marshal(map[string]any{
		"kty":     "oct",
		"alg":     alg,
		"ext":     true,
		"k":       base64.RawURLEncoding.EncodeToString(key),
		"key_ops": []string{"encrypt", "decrypt"},
	})
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func importOctJWK(value string) ([]byte, error) {
	var payload struct {
		K string `json:"k"`
	}
	if err := json.Unmarshal([]byte(value), &payload); err != nil {
		return nil, err
	}
	return base64.RawURLEncoding.DecodeString(payload.K)
}

func importOctJWKMap(value map[string]any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return importOctJWK(string(data))
}

func exportRSAPublicJWK(key *rsa.PublicKey) (string, error) {
	data, err := json.Marshal(map[string]any{
		"kty":     "RSA",
		"alg":     "RSA-OAEP-256",
		"e":       base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		"ext":     true,
		"key_ops": []string{"encrypt"},
		"n":       base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
	})
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func exportRSAPrivateJWK(key *rsa.PrivateKey) (string, error) {
	key.Precompute()
	data, err := json.Marshal(map[string]any{
		"kty":     "RSA",
		"alg":     "RSA-OAEP-256",
		"e":       base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		"ext":     true,
		"d":       base64.RawURLEncoding.EncodeToString(key.D.Bytes()),
		"dp":      base64.RawURLEncoding.EncodeToString(key.Precomputed.Dp.Bytes()),
		"dq":      base64.RawURLEncoding.EncodeToString(key.Precomputed.Dq.Bytes()),
		"key_ops": []string{"decrypt"},
		"n":       base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"p":       base64.RawURLEncoding.EncodeToString(key.Primes[0].Bytes()),
		"q":       base64.RawURLEncoding.EncodeToString(key.Primes[1].Bytes()),
		"qi":      base64.RawURLEncoding.EncodeToString(key.Precomputed.Qinv.Bytes()),
	})
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func importRSAPublicJWK(value string) (*rsa.PublicKey, error) {
	var payload struct {
		N string `json:"n"`
		E string `json:"e"`
	}
	if err := json.Unmarshal([]byte(value), &payload); err != nil {
		return nil, err
	}
	n, err := decodeBigInt(payload.N)
	if err != nil {
		return nil, err
	}
	e, err := decodeBigInt(payload.E)
	if err != nil {
		return nil, err
	}
	return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
}

func importRSAPrivateJWK(value string) (*rsa.PrivateKey, error) {
	var payload struct {
		N  string `json:"n"`
		E  string `json:"e"`
		D  string `json:"d"`
		P  string `json:"p"`
		Q  string `json:"q"`
		DP string `json:"dp"`
		DQ string `json:"dq"`
		QI string `json:"qi"`
	}
	if err := json.Unmarshal([]byte(value), &payload); err != nil {
		return nil, err
	}

	n, err := decodeBigInt(payload.N)
	if err != nil {
		return nil, err
	}
	e, err := decodeBigInt(payload.E)
	if err != nil {
		return nil, err
	}
	d, err := decodeBigInt(payload.D)
	if err != nil {
		return nil, err
	}
	p, err := decodeBigInt(payload.P)
	if err != nil {
		return nil, err
	}
	q, err := decodeBigInt(payload.Q)
	if err != nil {
		return nil, err
	}
	dp, err := decodeBigInt(payload.DP)
	if err != nil {
		return nil, err
	}
	dq, err := decodeBigInt(payload.DQ)
	if err != nil {
		return nil, err
	}
	qi, err := decodeBigInt(payload.QI)
	if err != nil {
		return nil, err
	}

	key := &rsa.PrivateKey{
		PublicKey: rsa.PublicKey{N: n, E: int(e.Int64())},
		D:         d,
		Primes:    []*big.Int{p, q},
		Precomputed: rsa.PrecomputedValues{
			Dp:   dp,
			Dq:   dq,
			Qinv: qi,
		},
	}
	if err := key.Validate(); err != nil {
		return nil, err
	}
	key.Precompute()
	return key, nil
}

func decodeBigInt(value string) (*big.Int, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(decoded), nil
}

func encryptFileBytes(name string, data []byte) (map[string]any, string, []byte, string, string, error) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, "", nil, "", "", err
	}
	iv := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return nil, "", nil, "", "", err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, "", nil, "", "", err
	}
	stream := cipher.NewCTR(block, iv)
	encrypted := make([]byte, len(data))
	stream.XORKeyStream(encrypted, data)

	jwk, err := exportOctJWK(key, "A256CTR")
	if err != nil {
		return nil, "", nil, "", "", err
	}
	var jwkMap map[string]any
	if err := json.Unmarshal([]byte(jwk), &jwkMap); err != nil {
		return nil, "", nil, "", "", err
	}

	hash := sha256.Sum256(data)
	nameHash := sha256.Sum256([]byte(name))
	return jwkMap, base64.StdEncoding.EncodeToString(iv), encrypted, fmt.Sprintf("%x", hash[:]), fmt.Sprintf("%x", nameHash[:]), nil
}

func encodePrefixedBase64(prefix string, data []byte) string {
	return prefix + base64.StdEncoding.EncodeToString(data)
}

func decodePrefixedBase64(value string) (string, []byte, error) {
	if len(value) < prefixedBase64EncodedBytes {
		return "", nil, errors.New("rocket: invalid prefixed base64 length")
	}
	prefix := value[:len(value)-prefixedBase64EncodedBytes]
	decoded, err := base64.StdEncoding.DecodeString(value[len(value)-prefixedBase64EncodedBytes:])
	if err != nil {
		return "", nil, err
	}
	if len(decoded) != prefixedBase64PayloadBytes {
		return "", nil, errors.New("rocket: invalid prefixed base64 payload length")
	}
	return prefix, decoded, nil
}

func cloneRoomState(state *e2eeRoomState) *e2eeRoomState {
	if state == nil {
		return nil
	}
	copy := *state
	if len(state.SessionKey) > 0 {
		copy.SessionKey = append([]byte(nil), state.SessionKey...)
	}
	if len(state.OldKeys) > 0 {
		copy.OldKeys = make(map[string][]byte, len(state.OldKeys))
		for keyID, keyBytes := range state.OldKeys {
			copy.OldKeys[keyID] = append([]byte(nil), keyBytes...)
		}
	}
	return &copy
}

func fileExtension(name string) string {
	parts := strings.Split(name, ".")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-1]
}
