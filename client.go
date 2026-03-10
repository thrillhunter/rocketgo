// Package rocket provides a stateful Rocket.Chat client with REST, DDP,
// session persistence, and end-to-end encryption support.
package rocket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultHTTPTimeout    = 30 * time.Second
	defaultInternalWait   = 30 * time.Second
	defaultPersistenceDir = ".rocket"
	reactionCacheLimit    = 2048
)

type Config struct {
	ServerURL            string
	Login                string
	Password             string
	UserID               string
	AuthToken            string
	DataDir              string
	PersistSession       bool
	AutoReconnect        bool
	HTTPClient           *http.Client
	AllowedDownloadHosts []string
	Logger               *slog.Logger
	E2EE                 E2EEConfig
}

type E2EEConfig struct {
	Enabled bool
	// Password unlocks the user's encrypted private key. For new accounts this
	// can be any stable string; for existing accounts use the recovery phrase.
	Password string
}

type Session struct {
	UserID    string    `json:"user_id"`
	AuthToken string    `json:"auth_token"`
	UpdatedAt time.Time `json:"updated_at"`
}

type TypingEvent struct {
	Time     time.Time
	Raw      json.RawMessage
	RoomID   string
	Username string
	Typing   bool
}

type UserActivityEvent struct {
	Time       time.Time
	Raw        json.RawMessage
	RoomID     string
	Username   string
	Activities []string
}

type DeleteEvent struct {
	Time      time.Time
	Raw       json.RawMessage
	RoomID    string
	MessageID string
}

type MessageReactionChange struct {
	Emoji    string
	Username string
	Added    bool
}

type MessageReactionEvent struct {
	Action    string
	Time      time.Time
	Raw       json.RawMessage
	Message   *Message
	RoomID    string
	MessageID string
	Changes   []MessageReactionChange
}

type NotificationEvent struct {
	Time    time.Time
	Raw     json.RawMessage
	Payload json.RawMessage
}

type RoomKeyRequestEvent struct {
	Time   time.Time
	Raw    json.RawMessage
	RoomID string
	KeyID  string
}

type UserRef struct {
	ID       string `json:"_id,omitempty"`
	Username string `json:"username,omitempty"`
	Name     string `json:"name,omitempty"`
}

type MessageReaction struct {
	Usernames []string `json:"usernames,omitempty"`
}

type Message struct {
	ID              string                     `json:"_id,omitempty"`
	RoomID          string                     `json:"rid,omitempty"`
	Text            string                     `json:"msg,omitempty"`
	Type            string                     `json:"t,omitempty"`
	ThreadMessageID string                     `json:"tmid,omitempty"`
	E2E             string                     `json:"e2e,omitempty"`
	Content         *EncryptedContent          `json:"content,omitempty"`
	Alias           string                     `json:"alias,omitempty"`
	Emoji           string                     `json:"emoji,omitempty"`
	Avatar          string                     `json:"avatar,omitempty"`
	Groupable       bool                       `json:"groupable,omitempty"`
	CustomFields    map[string]any             `json:"customFields,omitempty"`
	Timestamp       time.Time                  `json:"ts,omitempty"`
	UpdatedAt       time.Time                  `json:"_updatedAt,omitempty"`
	User            UserRef                    `json:"u,omitempty"`
	File            *File                      `json:"file,omitempty"`
	Files           []File                     `json:"files,omitempty"`
	Attachments     []Attachment               `json:"attachments,omitempty"`
	Reactions       map[string]MessageReaction `json:"reactions,omitempty"`
	Encrypted       bool                       `json:"-"`
	Decrypted       bool                       `json:"-"`
	DecryptError    string                     `json:"-"`
	Raw             json.RawMessage            `json:"-"`
}

type EncryptedContent struct {
	Algorithm  string `json:"algorithm,omitempty"`
	KeyID      string `json:"kid,omitempty"`
	IV         string `json:"iv,omitempty"`
	Ciphertext string `json:"ciphertext,omitempty"`
}

type File struct {
	ID        string `json:"_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Type      string `json:"type,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Format    string `json:"format,omitempty"`
	TypeGroup string `json:"typeGroup,omitempty"`
	URL       string `json:"url,omitempty"`
}

type Attachment struct {
	Title             string          `json:"title,omitempty"`
	Description       string          `json:"description,omitempty"`
	Type              string          `json:"type,omitempty"`
	TitleLink         string          `json:"title_link,omitempty"`
	TitleLinkDownload bool            `json:"title_link_download,omitempty"`
	FileID            string          `json:"fileId,omitempty"`
	Size              int64           `json:"size,omitempty"`
	Format            string          `json:"format,omitempty"`
	ImageURL          string          `json:"image_url,omitempty"`
	ImageType         string          `json:"image_type,omitempty"`
	ImageSize         int64           `json:"image_size,omitempty"`
	AudioURL          string          `json:"audio_url,omitempty"`
	AudioType         string          `json:"audio_type,omitempty"`
	AudioSize         int64           `json:"audio_size,omitempty"`
	VideoURL          string          `json:"video_url,omitempty"`
	VideoType         string          `json:"video_type,omitempty"`
	VideoSize         int64           `json:"video_size,omitempty"`
	Encryption        *FileEncryption `json:"encryption,omitempty"`
	Hashes            *FileHashes     `json:"hashes,omitempty"`
}

type FileEncryption struct {
	Key map[string]any `json:"key,omitempty"`
	IV  string         `json:"iv,omitempty"`
}

type FileHashes struct {
	SHA256 string `json:"sha256,omitempty"`
}

type Room struct {
	ID            string          `json:"_id,omitempty"`
	Name          string          `json:"name,omitempty"`
	FriendlyName  string          `json:"fname,omitempty"`
	Type          string          `json:"t,omitempty"`
	Encrypted     bool            `json:"encrypted,omitempty"`
	E2EKeyID      string          `json:"e2eKeyId,omitempty"`
	UpdatedAt     time.Time       `json:"_updatedAt,omitempty"`
	AvatarETag    string          `json:"avatarETag,omitempty"`
	LastMessage   *Message        `json:"lastMessage,omitempty"`
	UsersCount    int             `json:"usersCount,omitempty"`
	MessagesCount int             `json:"msgs,omitempty"`
	Raw           json.RawMessage `json:"-"`
}

type RoomKey struct {
	E2EKey   string    `json:"E2EKey,omitempty"`
	E2EKeyID string    `json:"e2eKeyId,omitempty"`
	TS       time.Time `json:"ts,omitempty"`
}

type Subscription struct {
	ID              string          `json:"_id,omitempty"`
	RoomID          string          `json:"rid,omitempty"`
	Name            string          `json:"name,omitempty"`
	FriendlyName    string          `json:"fname,omitempty"`
	Type            string          `json:"t,omitempty"`
	Open            bool            `json:"open,omitempty"`
	Unread          int             `json:"unread,omitempty"`
	Encrypted       bool            `json:"encrypted,omitempty"`
	E2EKey          string          `json:"E2EKey,omitempty"`
	E2ESuggestedKey string          `json:"E2ESuggestedKey,omitempty"`
	OldRoomKeys     []RoomKey       `json:"oldRoomKeys,omitempty"`
	UpdatedAt       time.Time       `json:"_updatedAt,omitempty"`
	User            UserRef         `json:"u,omitempty"`
	LastMessage     *Message        `json:"lastMessage,omitempty"`
	Raw             json.RawMessage `json:"-"`
}

type OutgoingMessage struct {
	ID              string
	RoomID          string
	Text            string
	ThreadMessageID string
	Alias           string
	Emoji           string
	Avatar          string
	Groupable       *bool
	CustomFields    map[string]any
}

type UploadFileParams struct {
	RoomID          string
	FileName        string
	ContentType     string
	Data            []byte
	Reader          io.Reader
	Description     string
	ThreadMessageID string
}

type Command struct {
	Prefix  string
	Name    string
	Args    []string
	RawArgs string
	Text    string
}

func ParseCommand(prefix, text string) (Command, bool) {
	prefix = strings.TrimSpace(prefix)
	text = strings.TrimSpace(text)
	if prefix == "" || text == "" || !strings.HasPrefix(text, prefix) {
		return Command{}, false
	}

	body := strings.TrimSpace(strings.TrimPrefix(text, prefix))
	if body == "" {
		return Command{}, false
	}

	fields := strings.Fields(body)
	if len(fields) == 0 {
		return Command{}, false
	}

	rawArgs := ""
	if len(fields) > 1 {
		rawArgs = strings.TrimSpace(body[len(fields[0]):])
	}

	return Command{
		Prefix:  prefix,
		Name:    strings.ToLower(fields[0]),
		Args:    append([]string(nil), fields[1:]...),
		RawArgs: rawArgs,
		Text:    text,
	}, true
}

func (m Message) ParseCommand(prefix string) (Command, bool) {
	return ParseCommand(prefix, m.Text)
}

func (m Message) ReactionUsernames(emoji string) []string {
	reaction, ok := m.Reactions[emoji]
	if !ok || len(reaction.Usernames) == 0 {
		return nil
	}
	return append([]string(nil), reaction.Usernames...)
}

func (m Message) HasReaction(emoji, username string) bool {
	for _, candidate := range m.ReactionUsernames(emoji) {
		if candidate == username {
			return true
		}
	}
	return false
}

type Client struct {
	cfg        Config
	log        *slog.Logger
	httpClient *http.Client
	baseURL    *url.URL
	restBase   string
	wsURL      string

	ddp  *ddpConn
	e2ee *e2eeManager

	closeOnce sync.Once
	closed    chan struct{}

	handlersMu            sync.RWMutex
	handlers              map[string][]*eventHandlerInstance
	onceHandlers          map[string][]*eventHandlerInstance
	interfaceHandlers     []*eventHandlerInstance
	interfaceOnceHandlers []*eventHandlerInstance

	mu          sync.RWMutex
	session     Session
	rooms       map[string]*Room
	subsByID    map[string]*Subscription
	subIDsByRid map[string]string
	watched     map[string]struct{}
	watchAll    bool

	messageReactions     map[string]map[string]MessageReaction
	messageReactionOrder []string
}

func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.ServerURL) == "" {
		return nil, errors.New("rocket: server url is required")
	}

	baseURL, err := url.Parse(strings.TrimSpace(cfg.ServerURL))
	if err != nil {
		return nil, fmt.Errorf("rocket: parse server url: %w", err)
	}
	if baseURL.Scheme != "http" && baseURL.Scheme != "https" {
		return nil, fmt.Errorf("rocket: unsupported scheme %q", baseURL.Scheme)
	}

	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.DataDir == "" && (cfg.E2EE.Enabled || cfg.PersistSession) {
		cfg.DataDir = defaultPersistenceDir
	}

	wsURL := websocketURL(baseURL)
	client := &Client{
		cfg:              cfg,
		log:              cfg.Logger.With("component", "rocket"),
		httpClient:       cfg.HTTPClient,
		baseURL:          baseURL,
		restBase:         strings.TrimRight(baseURL.String(), "/") + "/api/v1",
		wsURL:            wsURL,
		closed:           make(chan struct{}),
		rooms:            make(map[string]*Room),
		subsByID:         make(map[string]*Subscription),
		subIDsByRid:      make(map[string]string),
		watched:          make(map[string]struct{}),
		messageReactions: make(map[string]map[string]MessageReaction),
	}
	client.ddp = newDDPConn(client)
	client.e2ee = newE2EEManager(client)

	return client, nil
}

func (c *Client) Connect(ctx context.Context) error {
	c.log.Debug("connect: preparing persistence")
	if err := c.preparePersistence(); err != nil {
		return err
	}
	c.log.Debug("connect: loading persisted session and identity")
	_ = c.loadSessionFromDisk()
	_ = c.e2ee.loadIdentityFromDisk()

	c.log.Debug("connect: starting ddp connection")
	if err := c.ddp.connect(ctx); err != nil {
		return err
	}
	c.log.Debug("connect: ddp connected")

	c.log.Debug("connect: running post-login sync")
	if err := c.afterLoginSync(ctx); err != nil {
		_ = c.Close()
		return err
	}
	c.log.Debug("connect: post-login sync complete")

	if c.cfg.E2EE.Enabled {
		c.log.Debug("connect: initializing e2ee")
		if err := c.e2ee.init(ctx); err != nil {
			_ = c.Close()
			return err
		}
		c.log.Debug("connect: e2ee identity ready")
		c.log.Debug("connect: priming subscriptions")
		if err := c.e2ee.primeSubscriptions(ctx); err != nil {
			c.log.Debug("prime subscriptions failed", "error", err)
		}
		c.log.Debug("connect: subscription priming complete")
		c.log.Debug("connect: scheduling subscription key request")
		go func() {
			reqCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := c.e2ee.requestSubscriptionKeys(reqCtx); err != nil {
				c.log.Debug("request subscription keys failed", "error", err)
				return
			}
			c.log.Debug("connect: subscription key request complete")
		}()
	}

	c.emit(&ConnectEvent{Time: time.Now()})
	c.log.Debug("connect: ready")
	return nil
}

func (c *Client) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.closed)
		err = c.ddp.close()
	})
	return err
}

// Done is closed when the client shuts down.
func (c *Client) Done() <-chan struct{} {
	return c.closed
}

func (c *Client) Session() Session {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.session
}

func (c *Client) Rooms() []Room {
	c.mu.RLock()
	defer c.mu.RUnlock()

	rooms := make([]Room, 0, len(c.rooms))
	for _, room := range c.rooms {
		rooms = append(rooms, *cloneRoom(room))
	}
	sort.Slice(rooms, func(i, j int) bool { return rooms[i].ID < rooms[j].ID })
	return rooms
}

func (c *Client) RoomByID(roomID string) (*Room, bool) {
	roomID = strings.TrimSpace(roomID)
	if roomID == "" {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	room, ok := c.rooms[roomID]
	if !ok {
		return nil, false
	}
	return cloneRoom(room), true
}

func (c *Client) Subscriptions() []Subscription {
	c.mu.RLock()
	defer c.mu.RUnlock()

	subs := make([]Subscription, 0, len(c.subsByID))
	for _, sub := range c.subsByID {
		subs = append(subs, *cloneSubscription(sub))
	}
	sort.Slice(subs, func(i, j int) bool { return subs[i].ID < subs[j].ID })
	return subs
}

func (c *Client) SubscriptionByID(subscriptionID string) (*Subscription, bool) {
	subscriptionID = strings.TrimSpace(subscriptionID)
	if subscriptionID == "" {
		return nil, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	sub, ok := c.subsByID[subscriptionID]
	if !ok {
		return nil, false
	}
	return cloneSubscription(sub), true
}

func (c *Client) SubscriptionByRoomID(roomID string) (*Subscription, bool) {
	roomID = strings.TrimSpace(roomID)
	if roomID == "" {
		return nil, false
	}
	return c.getSubscriptionByRoomID(roomID)
}

func (c *Client) WatchJoinedRooms(ctx context.Context) error {
	c.mu.Lock()
	c.watchAll = true
	c.mu.Unlock()

	roomIDs := make(map[string]struct{})

	c.mu.RLock()
	for roomID := range c.subIDsByRid {
		roomID = strings.TrimSpace(roomID)
		if roomID != "" {
			roomIDs[roomID] = struct{}{}
		}
	}
	for roomID := range c.rooms {
		roomID = strings.TrimSpace(roomID)
		if roomID != "" {
			roomIDs[roomID] = struct{}{}
		}
	}
	c.mu.RUnlock()

	if len(roomIDs) == 0 {
		c.log.Debug("watch joined rooms: no cached rooms, refreshing subscriptions")
		subs, err := c.GetSubscriptions(ctx)
		if err != nil {
			return err
		}
		for _, sub := range subs {
			roomID := strings.TrimSpace(sub.RoomID)
			if roomID != "" {
				roomIDs[roomID] = struct{}{}
			}
		}
	}

	roomList := make([]string, 0, len(roomIDs))
	for roomID := range roomIDs {
		roomList = append(roomList, roomID)
	}
	sort.Strings(roomList)
	c.log.Debug("watch joined rooms: subscribing", "rooms", len(roomList))

	for _, roomID := range roomList {
		c.log.Debug("watch joined rooms: subscribing room", "room_id", roomID)
		if err := c.SubscribeRoom(ctx, roomID); err != nil {
			c.log.Error("watch joined rooms: subscribe failed", "room_id", roomID, "error", err)
			return err
		}
	}
	return nil
}

func (c *Client) SubscribeRoom(ctx context.Context, roomID string) error {
	roomID = strings.TrimSpace(roomID)
	if roomID == "" {
		return errors.New("rocket: room id is required")
	}

	if err := c.ddp.subscribe(ctx, "stream-room-messages", roomID, false); err != nil {
		return err
	}
	if err := c.ddp.subscribe(ctx, "stream-notify-room", roomID+"/typing", false); err != nil {
		return err
	}
	if err := c.ddp.subscribe(ctx, "stream-notify-room", roomID+"/deleteMessage", false); err != nil {
		return err
	}
	if err := c.ddp.subscribe(ctx, "stream-notify-room", roomID+"/user-activity", false); err != nil {
		return err
	}

	c.mu.Lock()
	c.watched[roomID] = struct{}{}
	c.mu.Unlock()
	return nil
}

func (c *Client) CreateDirectMessage(ctx context.Context, username string) (*Room, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, errors.New("rocket: username is required")
	}

	var resp struct {
		Success bool            `json:"success"`
		Room    json.RawMessage `json:"room"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/im.create", nil, map[string]string{"username": username}, &resp); err != nil {
		return nil, err
	}
	room, err := decodeRequiredRoom(resp.Room)
	if err != nil {
		return nil, err
	}
	c.upsertRoom(&room)
	c.mu.RLock()
	watchAll := c.watchAll
	c.mu.RUnlock()
	if watchAll {
		if err := c.SubscribeRoom(ctx, room.ID); err != nil {
			return nil, err
		}
	}
	return &room, nil
}

func (c *Client) ExecuteCommand(ctx context.Context, roomID, command, params string) error {
	roomID = strings.TrimSpace(roomID)
	command = strings.TrimSpace(command)
	if roomID == "" {
		return errors.New("rocket: room id is required")
	}
	if command == "" {
		return errors.New("rocket: command is required")
	}
	payload := map[string]string{
		"roomId":  roomID,
		"command": command,
		"params":  params,
	}
	return c.doJSON(ctx, http.MethodPost, "/commands.run", nil, payload, nil)
}

func (c *Client) SendText(ctx context.Context, roomID, text string) (*Message, error) {
	return c.SendMessage(ctx, OutgoingMessage{RoomID: roomID, Text: text})
}

func (c *Client) ReplyText(ctx context.Context, roomID, threadMessageID, text string) (*Message, error) {
	return c.SendMessage(ctx, OutgoingMessage{
		RoomID:          roomID,
		ThreadMessageID: threadMessageID,
		Text:            text,
	})
}

func (c *Client) SendMessage(ctx context.Context, msg OutgoingMessage) (*Message, error) {
	payload, err := c.buildOutgoingMessage(ctx, msg)
	if err != nil {
		return nil, err
	}
	result, err := c.ddp.call(ctx, "sendMessage", payload)
	if err != nil {
		return nil, err
	}
	message, err := decodeMessage(result)
	if err != nil {
		return nil, err
	}
	if err := c.processIncomingMessage(ctx, &message); err != nil {
		c.log.Debug("post-send message processing failed", "error", err)
	}
	c.rememberMessageReactions(&message)
	return &message, nil
}

func (c *Client) EditMessage(ctx context.Context, roomID, messageID, text string) (*Message, error) {
	payload, err := c.buildOutgoingMessage(ctx, OutgoingMessage{ID: messageID, RoomID: roomID, Text: text})
	if err != nil {
		return nil, err
	}
	result, err := c.ddp.call(ctx, "updateMessage", payload)
	if err != nil {
		return nil, err
	}
	if len(result) == 0 || string(result) == "null" {
		msg := &Message{ID: messageID, RoomID: roomID, Text: text}
		return msg, nil
	}
	message, err := decodeMessage(result)
	if err != nil {
		return nil, err
	}
	if err := c.processIncomingMessage(ctx, &message); err != nil {
		c.log.Debug("post-edit message processing failed", "error", err)
	}
	c.rememberMessageReactions(&message)
	return &message, nil
}

func (c *Client) DeleteMessage(ctx context.Context, messageID string) error {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return errors.New("rocket: message id is required")
	}
	_, err := c.ddp.call(ctx, "deleteMessage", map[string]string{"_id": messageID})
	if err == nil {
		c.removeMessageReactions(messageID)
	}
	return err
}

func (c *Client) SetReaction(ctx context.Context, messageID, emoji string, shouldReact bool) error {
	messageID = strings.TrimSpace(messageID)
	emoji = strings.TrimSpace(emoji)
	if messageID == "" {
		return errors.New("rocket: message id is required")
	}
	if emoji == "" {
		return errors.New("rocket: emoji is required")
	}
	payload := map[string]any{
		"messageId":   messageID,
		"emoji":       emoji,
		"shouldReact": shouldReact,
	}
	return c.doJSON(ctx, http.MethodPost, "/chat.react", nil, payload, nil)
}

func (c *Client) AddReaction(ctx context.Context, messageID, emoji string) error {
	return c.SetReaction(ctx, messageID, emoji, true)
}

func (c *Client) RemoveReaction(ctx context.Context, messageID, emoji string) error {
	return c.SetReaction(ctx, messageID, emoji, false)
}

func (c *Client) MarkRead(ctx context.Context, roomID string) error {
	roomID = strings.TrimSpace(roomID)
	if roomID == "" {
		return errors.New("rocket: room id is required")
	}
	if err := c.doJSON(ctx, http.MethodPost, "/subscriptions.read", nil, map[string]string{"rid": roomID}, nil); err != nil {
		return err
	}
	c.markRoomRead(roomID)
	return nil
}

func (c *Client) PinMessage(ctx context.Context, roomID, messageID string) error {
	roomID = strings.TrimSpace(roomID)
	messageID = strings.TrimSpace(messageID)
	if roomID == "" {
		return errors.New("rocket: room id is required")
	}
	if messageID == "" {
		return errors.New("rocket: message id is required")
	}
	_, err := c.ddp.call(ctx, "pinMessage", map[string]string{"_id": messageID, "rid": roomID})
	return err
}

func (c *Client) UnpinMessage(ctx context.Context, roomID, messageID string) error {
	roomID = strings.TrimSpace(roomID)
	messageID = strings.TrimSpace(messageID)
	if roomID == "" {
		return errors.New("rocket: room id is required")
	}
	if messageID == "" {
		return errors.New("rocket: message id is required")
	}
	_, err := c.ddp.call(ctx, "unpinMessage", map[string]string{"_id": messageID, "rid": roomID})
	return err
}

func (c *Client) StarMessage(ctx context.Context, roomID, messageID string, starred bool) error {
	roomID = strings.TrimSpace(roomID)
	messageID = strings.TrimSpace(messageID)
	if roomID == "" {
		return errors.New("rocket: room id is required")
	}
	if messageID == "" {
		return errors.New("rocket: message id is required")
	}
	_, err := c.ddp.call(ctx, "starMessage", map[string]any{"_id": messageID, "rid": roomID, "starred": starred})
	return err
}

func (c *Client) UnstarMessage(ctx context.Context, roomID, messageID string) error {
	return c.StarMessage(ctx, roomID, messageID, false)
}

func (c *Client) SetTyping(ctx context.Context, roomID, username string, typing bool) error {
	roomID = strings.TrimSpace(roomID)
	username = strings.TrimSpace(username)
	if roomID == "" {
		return errors.New("rocket: room id is required")
	}
	if username == "" {
		return errors.New("rocket: username is required")
	}
	_, err := c.ddp.call(ctx, "stream-notify-room", roomID+"/typing", username, typing)
	return err
}

func (c *Client) StartTyping(ctx context.Context, roomID, username string) error {
	return c.SetTyping(ctx, roomID, username, true)
}

func (c *Client) StopTyping(ctx context.Context, roomID, username string) error {
	return c.SetTyping(ctx, roomID, username, false)
}

func (c *Client) LoadHistory(ctx context.Context, roomID string) ([]Message, error) {
	result, err := c.ddp.call(ctx, "loadHistory", strings.TrimSpace(roomID))
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(result, &envelope); err != nil {
		return nil, err
	}
	messages := make([]Message, 0, len(envelope.Messages))
	for _, raw := range envelope.Messages {
		message, err := decodeMessage(raw)
		if err != nil {
			return nil, err
		}
		if err := c.processIncomingMessage(ctx, &message); err != nil {
			c.log.Debug("history message processing failed", "error", err)
		}
		c.rememberMessageReactions(&message)
		messages = append(messages, message)
	}
	return messages, nil
}

func (c *Client) UploadFile(ctx context.Context, params UploadFileParams) (*Message, error) {
	data := params.Data
	if data == nil && params.Reader != nil {
		var err error
		data, err = io.ReadAll(params.Reader)
		if err != nil {
			return nil, fmt.Errorf("rocket: read upload payload: %w", err)
		}
	}
	if len(data) == 0 {
		return nil, errors.New("rocket: upload data is empty")
	}
	if strings.TrimSpace(params.RoomID) == "" {
		return nil, errors.New("rocket: upload room id is required")
	}
	if strings.TrimSpace(params.FileName) == "" {
		return nil, errors.New("rocket: upload file name is required")
	}
	if strings.TrimSpace(params.ContentType) == "" {
		params.ContentType = "application/octet-stream"
	}

	return c.uploadFile(ctx, params, data)
}

func (c *Client) DownloadAttachment(ctx context.Context, attachment Attachment) ([]byte, error) {
	fileURL := attachmentURL(attachment)
	if fileURL == "" {
		return nil, errors.New("rocket: attachment does not contain a downloadable url")
	}
	data, err := c.downloadBytes(ctx, fileURL)
	if err != nil {
		return nil, err
	}
	if attachment.Encryption == nil {
		return data, nil
	}
	return c.e2ee.decryptAttachmentData(attachment, data)
}

func (c *Client) DownloadFile(ctx context.Context, file File) ([]byte, error) {
	fileURL := strings.TrimSpace(file.URL)
	if fileURL == "" {
		return nil, errors.New("rocket: file url is empty")
	}
	return c.downloadBytes(ctx, fileURL)
}

func (c *Client) DownloadMessageFile(ctx context.Context, message Message) ([]byte, error) {
	for _, attachment := range message.Attachments {
		if attachmentURL(attachment) == "" {
			continue
		}
		return c.DownloadAttachment(ctx, attachment)
	}
	if message.File != nil {
		return c.DownloadFile(ctx, *message.File)
	}
	if len(message.Files) > 0 {
		return c.DownloadFile(ctx, message.Files[0])
	}
	return nil, errors.New("rocket: message does not contain a downloadable file")
}

func (c *Client) GetRooms(ctx context.Context) ([]Room, error) {
	var resp struct {
		Success bool              `json:"success"`
		Update  []json.RawMessage `json:"update"`
		Remove  []string          `json:"remove"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/rooms.get", nil, nil, &resp); err != nil {
		return nil, err
	}
	rooms := make([]Room, 0, len(resp.Update))
	for _, raw := range resp.Update {
		room, err := decodeRoom(raw)
		if err != nil {
			return nil, err
		}
		c.upsertRoom(&room)
		rooms = append(rooms, room)
	}
	for _, roomID := range resp.Remove {
		c.removeRoom(strings.TrimSpace(roomID))
	}
	return rooms, nil
}

func (c *Client) GetSubscriptions(ctx context.Context) ([]Subscription, error) {
	var resp struct {
		Success bool              `json:"success"`
		Update  []json.RawMessage `json:"update"`
		Remove  []string          `json:"remove"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/subscriptions.get", nil, nil, &resp); err != nil {
		return nil, err
	}
	subs := make([]Subscription, 0, len(resp.Update))
	for _, raw := range resp.Update {
		sub, err := decodeSubscription(raw)
		if err != nil {
			return nil, err
		}
		c.upsertSubscription(&sub)
		subs = append(subs, sub)
	}
	for _, id := range resp.Remove {
		c.removeSubscription(strings.TrimSpace(id))
	}
	return subs, nil
}

func (c *Client) GetRoomInfo(ctx context.Context, roomID string) (*Room, error) {
	roomID = strings.TrimSpace(roomID)
	if roomID == "" {
		return nil, errors.New("rocket: room id is required")
	}
	query := url.Values{}
	query.Set("roomId", roomID)
	var resp struct {
		Success bool            `json:"success"`
		Room    json.RawMessage `json:"room"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/rooms.info", query, nil, &resp); err != nil {
		return nil, err
	}
	room, err := decodeRequiredRoom(resp.Room)
	if err != nil {
		return nil, err
	}
	c.upsertRoom(&room)
	return &room, nil
}

func (c *Client) GetMessage(ctx context.Context, messageID string) (*Message, error) {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return nil, errors.New("rocket: message id is required")
	}
	query := url.Values{}
	query.Set("msgId", messageID)
	var resp struct {
		Success bool            `json:"success"`
		Message json.RawMessage `json:"message"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/chat.getMessage", query, nil, &resp); err != nil {
		return nil, err
	}
	message, err := decodeRequiredMessage(resp.Message)
	if err != nil {
		return nil, err
	}
	if err := c.processIncomingMessage(ctx, &message); err != nil {
		c.log.Debug("fetched message processing failed", "error", err)
	}
	c.rememberMessageReactions(&message)
	return &message, nil
}

func (c *Client) SyncMessages(ctx context.Context, roomID string, lastUpdate time.Time) ([]Message, error) {
	roomID = strings.TrimSpace(roomID)
	if roomID == "" {
		return nil, errors.New("rocket: room id is required")
	}
	query := url.Values{}
	query.Set("roomId", roomID)
	query.Set("lastUpdate", lastUpdate.UTC().Format(time.RFC3339Nano))

	var resp struct {
		Success  bool              `json:"success"`
		Update   []json.RawMessage `json:"update"`
		Messages []json.RawMessage `json:"messages"`
		Result   struct {
			Updated []json.RawMessage `json:"updated"`
			Deleted []json.RawMessage `json:"deleted"`
		} `json:"result"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/chat.syncMessages", query, nil, &resp); err != nil {
		return nil, err
	}

	rawMessages := resp.Result.Updated
	if len(rawMessages) == 0 {
		rawMessages = resp.Messages
	}
	if len(rawMessages) == 0 {
		rawMessages = resp.Update
	}

	messages := make([]Message, 0, len(rawMessages))
	for _, raw := range rawMessages {
		message, err := decodeMessage(raw)
		if err != nil {
			return nil, err
		}
		if err := c.processIncomingMessage(ctx, &message); err != nil {
			c.log.Debug("sync message processing failed", "error", err)
		}
		c.rememberMessageReactions(&message)
		messages = append(messages, message)
	}
	return messages, nil
}

func (c *Client) buildOutgoingMessage(ctx context.Context, msg OutgoingMessage) (map[string]any, error) {
	msg.RoomID = strings.TrimSpace(msg.RoomID)
	if msg.RoomID == "" {
		return nil, errors.New("rocket: room id is required")
	}

	room, err := c.getOrFetchRoom(ctx, msg.RoomID)
	if err != nil {
		return nil, err
	}

	payload := map[string]any{
		"rid": msg.RoomID,
	}
	if msg.ID != "" {
		payload["_id"] = strings.TrimSpace(msg.ID)
	}
	if msg.ThreadMessageID != "" {
		payload["tmid"] = strings.TrimSpace(msg.ThreadMessageID)
	}
	if msg.Alias != "" {
		payload["alias"] = msg.Alias
	}
	if msg.Emoji != "" {
		payload["emoji"] = msg.Emoji
	}
	if msg.Avatar != "" {
		payload["avatar"] = msg.Avatar
	}
	if msg.Groupable != nil {
		payload["groupable"] = *msg.Groupable
	}
	if len(msg.CustomFields) > 0 {
		payload["customFields"] = msg.CustomFields
	}

	if room.Encrypted && !c.cfg.E2EE.Enabled {
		return nil, errors.New("rocket: room is encrypted but e2ee is disabled in the client config")
	}

	if room.Encrypted {
		encrypted, err := c.e2ee.encryptTextMessage(ctx, room, msg.Text)
		if err != nil {
			return nil, err
		}
		for k, v := range encrypted {
			payload[k] = v
		}
		return payload, nil
	}

	payload["msg"] = msg.Text
	return payload, nil
}

func (c *Client) afterLoginSync(ctx context.Context) error {
	if err := c.subscribeUserStreams(ctx); err != nil {
		return err
	}
	if _, err := c.GetRooms(ctx); err != nil {
		return err
	}
	if _, err := c.GetSubscriptions(ctx); err != nil {
		return err
	}
	return nil
}

func (c *Client) subscribeUserStreams(ctx context.Context) error {
	userID := c.Session().UserID
	if userID == "" {
		return errors.New("rocket: cannot subscribe without a logged in user")
	}
	keys := []string{
		userID + "/rooms-changed",
		userID + "/subscriptions-changed",
		userID + "/message",
		userID + "/notification",
	}
	if c.cfg.E2EE.Enabled {
		keys = append(keys, userID+"/e2ekeyRequest")
	}
	for _, key := range keys {
		if err := c.ddp.subscribe(ctx, "stream-notify-user", key, false); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) emit(event interface{}) {
	c.dispatchEvent(event)
}

func (c *Client) handleDisconnect(err error) {
	c.emit(&DisconnectEvent{Time: time.Now(), Err: err})
}

func (c *Client) handleDDPCollection(raw json.RawMessage, msg *ddpInbound) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultInternalWait)
	defer cancel()

	switch msg.Collection {
	case "stream-room-messages":
		if len(msg.Fields.Args) == 0 {
			return
		}
		when := time.Now()
		message, err := decodeMessage(msg.Fields.Args[0])
		if err != nil {
			c.emit(&ErrorEvent{Time: when, Raw: cloneRaw(raw), Err: err})
			return
		}
		if err := c.processIncomingMessage(ctx, &message); err != nil {
			c.log.Debug("incoming message processing failed", "error", err)
		}
		reaction := c.captureMessageReactionEvent(&message)
		c.emit(&MessageEvent{Action: msg.Msg, Time: when, Raw: cloneRaw(raw), Message: cloneMessage(&message)})
		if reaction != nil {
			reaction.Action = msg.Msg
			reaction.Time = when
			reaction.Raw = cloneRaw(raw)
			reaction.Message = cloneMessage(&message)
			c.emit(reaction)
		}
	case "stream-notify-room":
		c.handleRoomStreamEvent(raw, msg)
	case "stream-notify-user":
		c.handleUserStreamEvent(ctx, raw, msg)
	}
}

func (c *Client) handleRoomStreamEvent(raw json.RawMessage, msg *ddpInbound) {
	parts := strings.SplitN(msg.Fields.EventName, "/", 2)
	if len(parts) != 2 {
		return
	}
	roomID, kind := parts[0], parts[1]
	when := time.Now()

	switch kind {
	case "typing":
		if len(msg.Fields.Args) < 2 {
			return
		}
		var username string
		var typing bool
		_ = json.Unmarshal(msg.Fields.Args[0], &username)
		_ = json.Unmarshal(msg.Fields.Args[1], &typing)
		c.emit(&TypingEvent{Time: when, Raw: cloneRaw(raw), RoomID: roomID, Username: username, Typing: typing})
	case "deleteMessage":
		if len(msg.Fields.Args) == 0 {
			return
		}
		var payload struct {
			ID string `json:"_id"`
		}
		if err := json.Unmarshal(msg.Fields.Args[0], &payload); err != nil {
			c.emit(&ErrorEvent{Time: when, Raw: cloneRaw(raw), Err: err})
			return
		}
		c.removeMessageReactions(payload.ID)
		c.emit(&DeleteEvent{Time: when, Raw: cloneRaw(raw), RoomID: roomID, MessageID: payload.ID})
	case "user-activity":
		if len(msg.Fields.Args) < 2 {
			return
		}
		var payload UserActivityEvent
		payload.RoomID = roomID
		_ = json.Unmarshal(msg.Fields.Args[0], &payload.Username)
		_ = json.Unmarshal(msg.Fields.Args[1], &payload.Activities)
		payload.Time = when
		payload.Raw = cloneRaw(raw)
		c.emit(&payload)
	}
}

func (c *Client) handleUserStreamEvent(ctx context.Context, raw json.RawMessage, msg *ddpInbound) {
	parts := strings.SplitN(msg.Fields.EventName, "/", 2)
	if len(parts) != 2 {
		return
	}
	kind := parts[1]
	when := time.Now()

	switch kind {
	case "rooms-changed":
		if len(msg.Fields.Args) < 2 {
			return
		}
		var action string
		if err := json.Unmarshal(msg.Fields.Args[0], &action); err != nil {
			c.emit(&ErrorEvent{Time: when, Raw: cloneRaw(raw), Err: err})
			return
		}
		room, err := decodeRoomChange(msg.Fields.Args[1])
		if err != nil {
			c.emit(&ErrorEvent{Time: when, Raw: cloneRaw(raw), Err: err})
			return
		}
		if action == "removed" {
			c.removeRoom(room.ID)
		} else {
			c.upsertRoom(&room)
		}
		c.mu.RLock()
		_, watched := c.watched[room.ID]
		watchAll := c.watchAll
		c.mu.RUnlock()
		if action != "removed" && (watched || watchAll) {
			_ = c.SubscribeRoom(ctx, room.ID)
		}
		c.emit(&RoomEvent{Action: action, Time: when, Raw: cloneRaw(raw), Room: cloneRoom(&room)})
	case "subscriptions-changed":
		if len(msg.Fields.Args) < 2 {
			return
		}
		var action string
		if err := json.Unmarshal(msg.Fields.Args[0], &action); err != nil {
			c.emit(&ErrorEvent{Time: when, Raw: cloneRaw(raw), Err: err})
			return
		}
		sub, err := decodeSubscriptionChange(msg.Fields.Args[1])
		if err != nil {
			c.emit(&ErrorEvent{Time: when, Raw: cloneRaw(raw), Err: err})
			return
		}
		if action == "removed" {
			c.removeSubscription(sub.ID)
		} else {
			c.upsertSubscription(&sub)
		}
		if action != "removed" && c.cfg.E2EE.Enabled {
			if err := c.e2ee.handleSubscriptionUpdate(ctx, &sub); err != nil {
				c.log.Debug("subscription e2ee update failed", "error", err, "room_id", sub.RoomID)
			}
		}
		c.emit(&SubscriptionEvent{Action: action, Time: when, Raw: cloneRaw(raw), Subscription: cloneSubscription(&sub)})
	case "message":
		if len(msg.Fields.Args) == 0 {
			return
		}
		message, err := decodeMessage(msg.Fields.Args[0])
		if err != nil {
			c.emit(&ErrorEvent{Time: when, Raw: cloneRaw(raw), Err: err})
			return
		}
		if err := c.processIncomingMessage(ctx, &message); err != nil {
			c.log.Debug("user-stream message processing failed", "error", err)
		}
		reaction := c.captureMessageReactionEvent(&message)
		c.emit(&MessageEvent{Action: msg.Msg, Time: when, Raw: cloneRaw(raw), Message: cloneMessage(&message)})
		if reaction != nil {
			reaction.Action = msg.Msg
			reaction.Time = when
			reaction.Raw = cloneRaw(raw)
			reaction.Message = cloneMessage(&message)
			c.emit(reaction)
		}
	case "notification":
		if len(msg.Fields.Args) == 0 {
			return
		}
		payload := append(json.RawMessage(nil), msg.Fields.Args[0]...)
		c.emit(&NotificationEvent{Time: when, Raw: cloneRaw(raw), Payload: payload})
	case "e2ekeyRequest":
		if len(msg.Fields.Args) < 2 {
			return
		}
		var roomID, keyID string
		_ = json.Unmarshal(msg.Fields.Args[0], &roomID)
		_ = json.Unmarshal(msg.Fields.Args[1], &keyID)
		if roomID != "" && keyID != "" {
			if err := c.e2ee.handleKeyRequest(ctx, roomID, keyID); err != nil {
				c.log.Debug("room key request handling failed", "error", err, "room_id", roomID)
			}
			c.emit(&RoomKeyRequestEvent{Time: when, Raw: cloneRaw(raw), RoomID: roomID, KeyID: keyID})
		}
	}
}

func (c *Client) processIncomingMessage(ctx context.Context, message *Message) error {
	if message == nil {
		return nil
	}
	message.Raw = cloneRaw(message.Raw)
	if c.cfg.E2EE.Enabled {
		if err := c.e2ee.decryptMessage(ctx, message); err != nil {
			message.DecryptError = err.Error()
		}
	}
	return nil
}

func (c *Client) getOrFetchRoom(ctx context.Context, roomID string) (*Room, error) {
	c.mu.RLock()
	if room, ok := c.rooms[roomID]; ok {
		defer c.mu.RUnlock()
		return cloneRoom(room), nil
	}
	c.mu.RUnlock()
	return c.GetRoomInfo(ctx, roomID)
}

func (c *Client) getSubscriptionByRoomID(roomID string) (*Subscription, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	id, ok := c.subIDsByRid[roomID]
	if !ok {
		return nil, false
	}
	sub, ok := c.subsByID[id]
	if !ok {
		return nil, false
	}
	return cloneSubscription(sub), true
}

func (c *Client) upsertRoom(room *Room) {
	if room == nil || room.ID == "" {
		return
	}
	copy := *cloneRoom(room)
	c.mu.Lock()
	c.rooms[room.ID] = &copy
	c.mu.Unlock()
	if c.cfg.E2EE.Enabled {
		c.e2ee.handleRoomUpdate(&copy)
	}
}

func (c *Client) removeRoom(roomID string) {
	if roomID == "" {
		return
	}
	c.mu.Lock()
	delete(c.rooms, roomID)
	c.mu.Unlock()
	if c.cfg.E2EE.Enabled {
		c.e2ee.removeRoomState(roomID)
	}
}

func (c *Client) upsertSubscription(sub *Subscription) {
	if sub == nil || sub.ID == "" {
		return
	}
	copy := *cloneSubscription(sub)
	c.mu.Lock()
	c.subsByID[sub.ID] = &copy
	c.subIDsByRid[sub.RoomID] = sub.ID
	c.mu.Unlock()
}

func (c *Client) removeSubscription(id string) {
	if id == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if sub, ok := c.subsByID[id]; ok {
		delete(c.subIDsByRid, sub.RoomID)
		delete(c.subsByID, id)
		return
	}
	if subID, ok := c.subIDsByRid[id]; ok {
		delete(c.subIDsByRid, id)
		delete(c.subsByID, subID)
	}
}

func (c *Client) markRoomRead(roomID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	subID, ok := c.subIDsByRid[roomID]
	if !ok {
		return
	}
	sub, ok := c.subsByID[subID]
	if !ok || sub == nil {
		return
	}
	sub.Unread = 0
}

func (c *Client) rememberMessageReactions(message *Message) {
	_, _ = c.swapMessageReactions(message)
}

func (c *Client) captureMessageReactionEvent(message *Message) *MessageReactionEvent {
	previous, seen := c.swapMessageReactions(message)
	if !seen || message == nil || message.ID == "" {
		return nil
	}

	changes := diffMessageReactions(previous, message.Reactions)
	if len(changes) == 0 {
		return nil
	}
	return &MessageReactionEvent{
		RoomID:    message.RoomID,
		MessageID: message.ID,
		Changes:   changes,
	}
}

func (c *Client) swapMessageReactions(message *Message) (map[string]MessageReaction, bool) {
	if message == nil || strings.TrimSpace(message.ID) == "" {
		return nil, false
	}

	current := cloneMessageReactions(message.Reactions)

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.messageReactions == nil {
		c.messageReactions = make(map[string]map[string]MessageReaction)
	}

	previous, seen := c.messageReactions[message.ID]
	if !seen {
		c.messageReactionOrder = append(c.messageReactionOrder, message.ID)
	}
	c.messageReactions[message.ID] = current
	c.trimMessageReactionCacheLocked()
	return cloneMessageReactions(previous), seen
}

func (c *Client) removeMessageReactions(messageID string) {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return
	}
	c.mu.Lock()
	delete(c.messageReactions, messageID)
	c.mu.Unlock()
}

func (c *Client) trimMessageReactionCacheLocked() {
	for len(c.messageReactions) > reactionCacheLimit && len(c.messageReactionOrder) > 0 {
		oldest := c.messageReactionOrder[0]
		c.messageReactionOrder = c.messageReactionOrder[1:]
		delete(c.messageReactions, oldest)
	}
}

func (c *Client) setSession(session Session) {
	c.mu.Lock()
	c.session = session
	c.mu.Unlock()
	_ = c.saveSessionToDisk()
}

func (c *Client) preparePersistence() error {
	if c.cfg.DataDir == "" {
		return nil
	}
	return os.MkdirAll(c.cfg.DataDir, 0o700)
}

func (c *Client) sessionPath() string {
	if c.cfg.DataDir == "" || !c.cfg.PersistSession {
		return ""
	}
	return c.accountPersistencePath("session.json")
}

func (c *Client) saveSessionToDisk() error {
	path := c.sessionPath()
	if path == "" {
		return nil
	}
	session := c.Session()
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func (c *Client) loadSessionFromDisk() error {
	paths := c.persistencePathCandidates("session.json")
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

		var session Session
		if err := json.Unmarshal(data, &session); err != nil {
			return err
		}
		if session.UserID == "" || session.AuthToken == "" {
			return nil
		}

		c.setSession(session)
		if canonical := c.sessionPath(); canonical != "" && path != canonical {
			_ = os.Remove(path)
		}
		return nil
	}
	return nil
}

func (c *Client) accountPersistencePath(fileName string) string {
	if c.cfg.DataDir == "" {
		return ""
	}
	return filepath.Join(c.cfg.DataDir, c.persistenceServerKey(), c.persistenceAccountKey(), fileName)
}

func (c *Client) legacyPersistencePath(fileName string) string {
	if c.cfg.DataDir == "" {
		return ""
	}
	return filepath.Join(c.cfg.DataDir, fileName)
}

func (c *Client) persistencePathCandidates(fileName string) []string {
	paths := make([]string, 0, 2)
	for _, path := range []string{
		c.accountPersistencePath(fileName),
		c.legacyPersistencePath(fileName),
	} {
		if path == "" {
			continue
		}
		duplicate := false
		for _, existing := range paths {
			if existing == path {
				duplicate = true
				break
			}
		}
		if !duplicate {
			paths = append(paths, path)
		}
	}
	return paths
}

func (c *Client) persistenceServerKey() string {
	server := strings.ToLower(strings.TrimSpace(c.baseURL.Host))
	if basePath := strings.Trim(strings.ToLower(c.baseURL.Path), "/"); basePath != "" {
		server += "_" + strings.ReplaceAll(basePath, "/", "_")
	}
	return sanitizePersistencePart(server, "server")
}

func (c *Client) persistenceAccountKey() string {
	switch {
	case strings.TrimSpace(c.cfg.UserID) != "":
		return "uid-" + sanitizePersistencePart(c.cfg.UserID, "account")
	case strings.TrimSpace(c.cfg.Login) != "":
		return "login-" + sanitizePersistencePart(strings.ToLower(c.cfg.Login), "account")
	default:
		session := c.Session()
		if strings.TrimSpace(session.UserID) != "" {
			return "uid-" + sanitizePersistencePart(session.UserID, "account")
		}
		return "default"
	}
}

func sanitizePersistencePart(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}

	var builder strings.Builder
	lastUnderscore := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
			lastUnderscore = false
		case r >= 'A' && r <= 'Z':
			builder.WriteRune(r + ('a' - 'A'))
			lastUnderscore = false
		case r == '-', r == '_', r == '.':
			builder.WriteRune(r)
			lastUnderscore = false
		default:
			if !lastUnderscore {
				builder.WriteByte('_')
				lastUnderscore = true
			}
		}
	}

	result := strings.Trim(builder.String(), "._-")
	if result == "" {
		return fallback
	}
	return result
}

func websocketURL(baseURL *url.URL) string {
	ws := *baseURL
	if ws.Scheme == "https" {
		ws.Scheme = "wss"
	} else {
		ws.Scheme = "ws"
	}
	ws.Path = strings.TrimRight(ws.Path, "/") + "/websocket"
	ws.RawQuery = ""
	ws.Fragment = ""
	return ws.String()
}

func cloneRoom(room *Room) *Room {
	if room == nil {
		return nil
	}
	copy := *room
	if room.LastMessage != nil {
		msg := cloneMessage(room.LastMessage)
		copy.LastMessage = msg
	}
	copy.Raw = cloneRaw(room.Raw)
	return &copy
}

func cloneSubscription(sub *Subscription) *Subscription {
	if sub == nil {
		return nil
	}
	copy := *sub
	if len(sub.OldRoomKeys) > 0 {
		copy.OldRoomKeys = append([]RoomKey(nil), sub.OldRoomKeys...)
	}
	if sub.LastMessage != nil {
		copy.LastMessage = cloneMessage(sub.LastMessage)
	}
	copy.Raw = cloneRaw(sub.Raw)
	return &copy
}

func cloneMessage(msg *Message) *Message {
	if msg == nil {
		return nil
	}
	copy := *msg
	if msg.Content != nil {
		content := *msg.Content
		copy.Content = &content
	}
	if len(msg.Attachments) > 0 {
		copy.Attachments = append([]Attachment(nil), msg.Attachments...)
	}
	if len(msg.Files) > 0 {
		copy.Files = append([]File(nil), msg.Files...)
	}
	if len(msg.Reactions) > 0 {
		copy.Reactions = cloneMessageReactions(msg.Reactions)
	}
	if msg.File != nil {
		file := *msg.File
		copy.File = &file
	}
	if len(msg.CustomFields) > 0 {
		copy.CustomFields = make(map[string]any, len(msg.CustomFields))
		for k, v := range msg.CustomFields {
			copy.CustomFields[k] = v
		}
	}
	copy.Raw = cloneRaw(msg.Raw)
	return &copy
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func decodeRoomChange(raw json.RawMessage) (Room, error) {
	room, err := decodeRoom(raw)
	if err == nil {
		return room, nil
	}

	var id string
	if unmarshalErr := json.Unmarshal(raw, &id); unmarshalErr == nil && id != "" {
		return Room{ID: id, Raw: cloneRaw(raw)}, nil
	}
	return Room{}, err
}

func decodeSubscriptionChange(raw json.RawMessage) (Subscription, error) {
	sub, err := decodeSubscription(raw)
	if err == nil {
		return sub, nil
	}

	var id string
	if unmarshalErr := json.Unmarshal(raw, &id); unmarshalErr == nil && id != "" {
		return Subscription{ID: id, Raw: cloneRaw(raw)}, nil
	}
	return Subscription{}, err
}

func cloneMessageReactions(reactions map[string]MessageReaction) map[string]MessageReaction {
	if len(reactions) == 0 {
		return map[string]MessageReaction{}
	}

	copy := make(map[string]MessageReaction, len(reactions))
	for emoji, reaction := range reactions {
		copy[emoji] = MessageReaction{
			Usernames: append([]string(nil), reaction.Usernames...),
		}
	}
	return copy
}

func diffMessageReactions(previous, current map[string]MessageReaction) []MessageReactionChange {
	if previous == nil {
		previous = map[string]MessageReaction{}
	}
	if current == nil {
		current = map[string]MessageReaction{}
	}

	emojis := make([]string, 0, len(previous)+len(current))
	seen := make(map[string]struct{}, len(previous)+len(current))
	for emoji := range previous {
		seen[emoji] = struct{}{}
		emojis = append(emojis, emoji)
	}
	for emoji := range current {
		if _, ok := seen[emoji]; ok {
			continue
		}
		emojis = append(emojis, emoji)
	}
	sort.Strings(emojis)

	changes := make([]MessageReactionChange, 0)
	for _, emoji := range emojis {
		prevUsers := usernamesSet(previous[emoji].Usernames)
		currUsers := usernamesSet(current[emoji].Usernames)

		for _, username := range sortedSetDifference(prevUsers, currUsers) {
			changes = append(changes, MessageReactionChange{
				Emoji:    emoji,
				Username: username,
				Added:    false,
			})
		}
		for _, username := range sortedSetDifference(currUsers, prevUsers) {
			changes = append(changes, MessageReactionChange{
				Emoji:    emoji,
				Username: username,
				Added:    true,
			})
		}
	}
	return changes
}

func usernamesSet(usernames []string) map[string]struct{} {
	set := make(map[string]struct{}, len(usernames))
	for _, username := range usernames {
		username = strings.TrimSpace(username)
		if username == "" {
			continue
		}
		set[username] = struct{}{}
	}
	return set
}

func sortedSetDifference(left, right map[string]struct{}) []string {
	result := make([]string, 0)
	for value := range left {
		if _, ok := right[value]; ok {
			continue
		}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func attachmentURL(attachment Attachment) string {
	for _, candidate := range []string{
		attachment.TitleLink,
		attachment.ImageURL,
		attachment.AudioURL,
		attachment.VideoURL,
	} {
		if url := strings.TrimSpace(candidate); url != "" {
			return url
		}
	}
	return ""
}
