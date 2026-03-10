package rocket

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type apiError struct {
	Success   bool   `json:"success"`
	Status    string `json:"status"`
	Error     string `json:"error"`
	ErrorType string `json:"errorType"`
	Message   string `json:"message"`
}

func (c *Client) doJSON(ctx context.Context, method, endpoint string, query url.Values, body any, dest any) error {
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("rocket: marshal %s %s: %w", method, endpoint, err)
		}
		payload = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.restURL(endpoint, query), payload)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.applyAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeAPIError(resp.StatusCode, data)
	}
	if err := decodeAPISuccess(data); err != nil {
		return err
	}
	if dest == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, dest); err != nil {
		return fmt.Errorf("rocket: decode %s %s: %w", method, endpoint, err)
	}
	return nil
}

func (c *Client) uploadMultipart(ctx context.Context, endpoint string, fileName, contentType string, data []byte, fields map[string]string, dest any) error {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	for k, v := range fields {
		if err := writer.WriteField(k, v); err != nil {
			return err
		}
	}

	part, err := writer.CreateFormFile("file", fileName)
	if err != nil {
		return err
	}
	if _, err := part.Write(data); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.restURL(endpoint, nil), &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("X-File-Content-Type", contentType)
	}
	c.applyAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeAPIError(resp.StatusCode, respBody)
	}
	if err := decodeAPISuccess(respBody); err != nil {
		return err
	}
	if dest == nil || len(respBody) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, dest); err != nil {
		return fmt.Errorf("rocket: decode upload response: %w", err)
	}
	return nil
}

func (c *Client) downloadBytes(ctx context.Context, fileURL string) ([]byte, error) {
	targetURL, err := c.resolveDownloadURL(fileURL)
	if err != nil {
		return nil, err
	}

	sameOrigin := sameDownloadOrigin(c.baseURL, targetURL)
	if !sameOrigin && !c.isAllowedDownloadHost(targetURL) {
		return nil, fmt.Errorf("rocket: download host %q is not allowed", targetURL.Host)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL.String(), nil)
	if err != nil {
		return nil, err
	}
	if sameOrigin {
		c.applyAuth(req)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, decodeAPIError(resp.StatusCode, body)
	}
	return io.ReadAll(resp.Body)
}

func (c *Client) resolveDownloadURL(fileURL string) (*url.URL, error) {
	fileURL = strings.TrimSpace(fileURL)
	if fileURL == "" {
		return nil, errors.New("rocket: download url is empty")
	}

	parsed, err := url.Parse(fileURL)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme == "" {
		base := *c.baseURL
		if !strings.HasSuffix(base.Path, "/") {
			base.Path += "/"
		}
		parsed = base.ResolveReference(parsed)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		return parsed, nil
	default:
		return nil, fmt.Errorf("rocket: unsupported download url scheme %q", parsed.Scheme)
	}
}

func (c *Client) isAllowedDownloadHost(targetURL *url.URL) bool {
	for _, allowed := range c.cfg.AllowedDownloadHosts {
		if matchesAllowedDownloadHost(targetURL, allowed) {
			return true
		}
	}
	return false
}

func sameDownloadOrigin(baseURL, targetURL *url.URL) bool {
	if baseURL == nil || targetURL == nil {
		return false
	}
	return strings.EqualFold(baseURL.Scheme, targetURL.Scheme) &&
		strings.EqualFold(baseURL.Hostname(), targetURL.Hostname()) &&
		effectiveURLPort(baseURL) == effectiveURLPort(targetURL)
}

func matchesAllowedDownloadHost(targetURL *url.URL, allowed string) bool {
	if targetURL == nil {
		return false
	}

	allowed = strings.TrimSpace(allowed)
	if allowed == "" {
		return false
	}

	if strings.Contains(allowed, "://") {
		parsed, err := url.Parse(allowed)
		if err != nil || parsed.Host == "" {
			return false
		}
		if parsed.Scheme != "" && !strings.EqualFold(parsed.Scheme, targetURL.Scheme) {
			return false
		}
		return sameDownloadHost(targetURL, parsed.Host)
	}

	return sameDownloadHost(targetURL, allowed)
}

func sameDownloadHost(targetURL *url.URL, host string) bool {
	parsed, err := url.Parse("//" + strings.TrimSpace(host))
	if err != nil || parsed.Host == "" {
		return false
	}
	if !strings.EqualFold(parsed.Hostname(), targetURL.Hostname()) {
		return false
	}
	if parsed.Port() == "" {
		return true
	}
	return parsed.Port() == effectiveURLPort(targetURL)
}

func effectiveURLPort(value *url.URL) string {
	if value == nil {
		return ""
	}
	if port := value.Port(); port != "" {
		return port
	}
	switch strings.ToLower(value.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

func (c *Client) uploadFile(ctx context.Context, params UploadFileParams, data []byte) (*Message, error) {
	room, err := c.getOrFetchRoom(ctx, params.RoomID)
	if err != nil {
		return nil, err
	}
	if room.Encrypted && !c.cfg.E2EE.Enabled {
		return nil, errors.New("rocket: room is encrypted but e2ee is disabled in the client config")
	}

	payload := uploadPayload{
		FileName:    params.FileName,
		ContentType: params.ContentType,
		UploadBytes: data,
	}
	if room.Encrypted && c.cfg.E2EE.Enabled {
		encrypted, err := c.e2ee.prepareUpload(ctx, room, params.FileName, params.ContentType, data, params.Description)
		if err != nil {
			return nil, err
		}
		payload = *encrypted
	}

	var uploadResp struct {
		Success bool `json:"success"`
		File    struct {
			ID   string `json:"_id"`
			URL  string `json:"url"`
			Name string `json:"name"`
			Type string `json:"type"`
			Size int64  `json:"size"`
		} `json:"file"`
	}
	fields := map[string]string{}
	if payload.EncryptedFileContent != nil {
		encoded, err := json.Marshal(payload.EncryptedFileContent)
		if err != nil {
			return nil, err
		}
		fields["content"] = string(encoded)
	}
	if err := c.uploadMultipart(ctx, "/rooms.media/"+url.PathEscape(params.RoomID), payload.FileName, payload.ContentType, payload.UploadBytes, fields, &uploadResp); err != nil {
		return nil, err
	}
	if strings.TrimSpace(uploadResp.File.ID) == "" {
		return nil, errors.New("rocket: upload response missing file id")
	}
	uploadedFile := File{
		ID:        uploadResp.File.ID,
		Name:      firstNonEmpty(uploadResp.File.Name, params.FileName),
		Type:      firstNonEmpty(uploadResp.File.Type, params.ContentType),
		Size:      uploadResp.File.Size,
		Format:    fileExtension(firstNonEmpty(uploadResp.File.Name, params.FileName)),
		TypeGroup: contentTypeGroup(firstNonEmpty(uploadResp.File.Type, params.ContentType)),
		URL:       uploadResp.File.URL,
	}
	if payload.PlainFile == nil {
		payload.PlainFile = &uploadedFile
	}

	confirm := map[string]any{}
	if params.Description != "" {
		confirm["description"] = params.Description
	}
	if params.ThreadMessageID != "" {
		confirm["tmid"] = params.ThreadMessageID
	}
	if payload.MessageType != "" {
		confirm["t"] = payload.MessageType
	}
	if payload.EncryptedFile != nil {
		if strings.TrimSpace(uploadResp.File.URL) == "" {
			return nil, errors.New("rocket: encrypted upload response missing file url")
		}
		content, fileRef, err := c.e2ee.buildEncryptedUploadMessage(ctx, room, uploadResp.File.ID, uploadResp.File.URL, payload.EncryptedFile)
		if err != nil {
			return nil, err
		}
		confirm["content"] = content
		payload.PlainFile = fileRef
	}

	var confirmResp struct {
		Success bool     `json:"success"`
		Message *Message `json:"message"`
	}
	endpoint := fmt.Sprintf("/rooms.mediaConfirm/%s/%s", url.PathEscape(params.RoomID), url.PathEscape(uploadResp.File.ID))
	if err := c.doJSON(ctx, http.MethodPost, endpoint, nil, confirm, &confirmResp); err != nil {
		return nil, err
	}
	if confirmResp.Message != nil {
		if err := c.processIncomingMessage(ctx, confirmResp.Message); err != nil {
			c.log.Debug("upload confirm message processing failed", "error", err)
		}
		c.rememberMessageReactions(confirmResp.Message)
		return confirmResp.Message, nil
	}

	message := &Message{RoomID: params.RoomID, Type: payload.MessageType, Text: params.Description}
	if payload.PlainFile != nil {
		message.File = payload.PlainFile
		message.Files = []File{*payload.PlainFile}
	}
	return message, nil
}

func (c *Client) restURL(endpoint string, query url.Values) string {
	full := strings.TrimRight(c.restBase, "/") + endpoint
	if len(query) == 0 {
		return full
	}
	return full + "?" + query.Encode()
}

func (c *Client) applyAuth(req *http.Request) {
	session := c.Session()
	if session.AuthToken != "" {
		req.Header.Set("X-Auth-Token", session.AuthToken)
	}
	if session.UserID != "" {
		req.Header.Set("X-User-Id", session.UserID)
	}
}

func decodeAPIError(statusCode int, data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("rocket: http %d", statusCode)
	}
	var apiErr apiError
	if err := json.Unmarshal(data, &apiErr); err == nil {
		if apiErr.Error != "" {
			return fmt.Errorf("rocket: http %d: %s", statusCode, apiErr.Error)
		}
		if apiErr.Message != "" {
			return fmt.Errorf("rocket: http %d: %s", statusCode, apiErr.Message)
		}
	}
	return fmt.Errorf("rocket: http %d: %s", statusCode, strings.TrimSpace(string(data)))
}

func decodeAPISuccess(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	var envelope struct {
		Success *bool  `json:"success"`
		Error   string `json:"error"`
		Message string `json:"message"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil || envelope.Success == nil || *envelope.Success {
		return nil
	}
	switch {
	case envelope.Error != "":
		return fmt.Errorf("rocket: %s", envelope.Error)
	case envelope.Message != "":
		return fmt.Errorf("rocket: %s", envelope.Message)
	case envelope.Status != "":
		return fmt.Errorf("rocket: %s", envelope.Status)
	default:
		return errors.New("rocket: api request failed")
	}
}

func decodeMessage(raw json.RawMessage) (Message, error) {
	var wire struct {
		ID              string                     `json:"_id"`
		RoomID          string                     `json:"rid"`
		Text            string                     `json:"msg"`
		Type            string                     `json:"t"`
		ThreadMessageID string                     `json:"tmid"`
		E2E             string                     `json:"e2e"`
		Content         json.RawMessage            `json:"content"`
		Alias           string                     `json:"alias"`
		Emoji           string                     `json:"emoji"`
		Avatar          string                     `json:"avatar"`
		Groupable       bool                       `json:"groupable"`
		CustomFields    map[string]any             `json:"customFields"`
		TS              json.RawMessage            `json:"ts"`
		UpdatedAt       json.RawMessage            `json:"_updatedAt"`
		User            UserRef                    `json:"u"`
		File            *File                      `json:"file"`
		Files           []File                     `json:"files"`
		Attachments     []Attachment               `json:"attachments"`
		Reactions       map[string]MessageReaction `json:"reactions"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Message{}, err
	}
	msg := Message{
		ID:              wire.ID,
		RoomID:          wire.RoomID,
		Text:            wire.Text,
		Type:            wire.Type,
		ThreadMessageID: wire.ThreadMessageID,
		E2E:             wire.E2E,
		Alias:           wire.Alias,
		Emoji:           wire.Emoji,
		Avatar:          wire.Avatar,
		Groupable:       wire.Groupable,
		CustomFields:    wire.CustomFields,
		User:            wire.User,
		File:            wire.File,
		Files:           wire.Files,
		Attachments:     wire.Attachments,
		Reactions:       wire.Reactions,
		Raw:             cloneRaw(raw),
	}
	if len(wire.Content) > 0 && string(wire.Content) != "null" {
		var content EncryptedContent
		if err := json.Unmarshal(wire.Content, &content); err == nil {
			msg.Content = &content
		}
	}
	msg.Timestamp, _ = parseJSONTime(wire.TS)
	msg.UpdatedAt, _ = parseJSONTime(wire.UpdatedAt)
	return msg, nil
}

func decodeRoom(raw json.RawMessage) (Room, error) {
	var wire struct {
		ID           string          `json:"_id"`
		Name         string          `json:"name"`
		FriendlyName string          `json:"fname"`
		Type         string          `json:"t"`
		Encrypted    bool            `json:"encrypted"`
		E2EKeyID     string          `json:"e2eKeyId"`
		UpdatedAt    json.RawMessage `json:"_updatedAt"`
		AvatarETag   string          `json:"avatarETag"`
		UsersCount   int             `json:"usersCount"`
		Messages     int             `json:"msgs"`
		LastMessage  json.RawMessage `json:"lastMessage"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Room{}, err
	}
	room := Room{
		ID:            wire.ID,
		Name:          wire.Name,
		FriendlyName:  wire.FriendlyName,
		Type:          wire.Type,
		Encrypted:     wire.Encrypted,
		E2EKeyID:      wire.E2EKeyID,
		AvatarETag:    wire.AvatarETag,
		UsersCount:    wire.UsersCount,
		MessagesCount: wire.Messages,
		Raw:           cloneRaw(raw),
	}
	room.UpdatedAt, _ = parseJSONTime(wire.UpdatedAt)
	if len(wire.LastMessage) > 0 && string(wire.LastMessage) != "null" {
		msg, err := decodeMessage(wire.LastMessage)
		if err != nil {
			return Room{}, err
		}
		room.LastMessage = &msg
	}
	return room, nil
}

func decodeSubscription(raw json.RawMessage) (Subscription, error) {
	var wire struct {
		ID              string `json:"_id"`
		RoomID          string `json:"rid"`
		Name            string `json:"name"`
		FriendlyName    string `json:"fname"`
		Type            string `json:"t"`
		Open            bool   `json:"open"`
		Unread          int    `json:"unread"`
		Encrypted       bool   `json:"encrypted"`
		E2EKey          string `json:"E2EKey"`
		E2ESuggestedKey string `json:"E2ESuggestedKey"`
		OldRoomKeys     []struct {
			E2EKey   string          `json:"E2EKey"`
			E2EKeyID string          `json:"e2eKeyId"`
			TS       json.RawMessage `json:"ts"`
		} `json:"oldRoomKeys"`
		UpdatedAt   json.RawMessage `json:"_updatedAt"`
		User        UserRef         `json:"u"`
		LastMessage json.RawMessage `json:"lastMessage"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return Subscription{}, err
	}
	sub := Subscription{
		ID:              wire.ID,
		RoomID:          wire.RoomID,
		Name:            wire.Name,
		FriendlyName:    wire.FriendlyName,
		Type:            wire.Type,
		Open:            wire.Open,
		Unread:          wire.Unread,
		Encrypted:       wire.Encrypted,
		E2EKey:          wire.E2EKey,
		E2ESuggestedKey: wire.E2ESuggestedKey,
		User:            wire.User,
		Raw:             cloneRaw(raw),
	}
	sub.UpdatedAt, _ = parseJSONTime(wire.UpdatedAt)
	if len(wire.OldRoomKeys) > 0 {
		sub.OldRoomKeys = make([]RoomKey, 0, len(wire.OldRoomKeys))
		for _, old := range wire.OldRoomKeys {
			ts, _ := parseJSONTime(old.TS)
			sub.OldRoomKeys = append(sub.OldRoomKeys, RoomKey{E2EKey: old.E2EKey, E2EKeyID: old.E2EKeyID, TS: ts})
		}
	}
	if len(wire.LastMessage) > 0 && string(wire.LastMessage) != "null" {
		msg, err := decodeMessage(wire.LastMessage)
		if err != nil {
			return Subscription{}, err
		}
		sub.LastMessage = &msg
	}
	return sub, nil
}

func parseJSONTime(raw json.RawMessage) (time.Time, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}, nil
	}
	var direct string
	if err := json.Unmarshal(raw, &direct); err == nil && direct != "" {
		return parseTimeString(direct)
	}
	var millis float64
	if err := json.Unmarshal(raw, &millis); err == nil {
		return time.UnixMilli(int64(millis)), nil
	}
	var wrapped struct {
		Date json.RawMessage `json:"$date"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && len(wrapped.Date) > 0 {
		if err := json.Unmarshal(wrapped.Date, &direct); err == nil && direct != "" {
			return parseTimeString(direct)
		}
		if err := json.Unmarshal(wrapped.Date, &millis); err == nil {
			return time.UnixMilli(int64(millis)), nil
		}
	}
	return time.Time{}, errors.New("rocket: unsupported time format")
}

func parseTimeString(value string) (time.Time, error) {
	layouts := []string{time.RFC3339Nano, time.RFC3339}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("rocket: parse time %q", value)
}

func decodeRequiredRoom(raw json.RawMessage) (Room, error) {
	if isNullJSON(raw) {
		return Room{}, errors.New("rocket: room missing in response")
	}
	room, err := decodeRoom(raw)
	if err != nil {
		return Room{}, err
	}
	if room.ID == "" {
		return Room{}, errors.New("rocket: room id missing in response")
	}
	return room, nil
}

func decodeRequiredMessage(raw json.RawMessage) (Message, error) {
	if isNullJSON(raw) {
		return Message{}, errors.New("rocket: message missing in response")
	}
	message, err := decodeMessage(raw)
	if err != nil {
		return Message{}, err
	}
	if message.ID == "" {
		return Message{}, errors.New("rocket: message id missing in response")
	}
	return message, nil
}

func isNullJSON(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func contentTypeGroup(value string) string {
	parts := strings.SplitN(strings.TrimSpace(value), "/", 2)
	if len(parts) == 2 && parts[0] != "" {
		return parts[0]
	}
	return strings.TrimSpace(value)
}
