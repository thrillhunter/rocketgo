package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	rocket "github.com/thrillhunter21/rocketgo"
)

var config = rocket.Config{
	ServerURL:      "https://chat.example.com",
	Login:          "bot-username",
	Password:       "bot-password",
	DataDir:        ".rocket",
	PersistSession: true,
	AutoReconnect:  true,
	Logger:         nil,
	E2EE: rocket.E2EEConfig{
		Enabled:  true,
		Password: "set-your-e2ee-password-or-12-word-recovery-phrase",
	},
}

const roomID = ""

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	config.Logger = slog.Default()

	client, err := rocket.New(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create client: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	ctx := context.Background()

	if err := client.Connect(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "connect failed: %v\n", err)
		os.Exit(1)
	}

	if roomID == "" {
		printJoinedRooms(client.Subscriptions())
		return
	}

	// with e2ee enabled correctly, encrypted messages are decrypted into Message.Text.
	messages, err := client.LoadHistory(ctx, roomID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load history failed: %v\n", err)
		os.Exit(1)
	}

	for _, message := range messages {
		fmt.Printf("%s %s: %s\n", message.Timestamp.Format("2006-01-02 15:04:05"), messageAuthor(message), renderMessage(message))
	}
}

func printJoinedRooms(subs []rocket.Subscription) {
	if len(subs) == 0 {
		fmt.Println("No joined rooms found.")
		return
	}

	fmt.Println("Set roomID to one of these joined rooms:")
	for _, sub := range subs {
		fmt.Printf("- %s (%s, encrypted=%t)\n", subscriptionName(sub), sub.RoomID, sub.Encrypted)
	}
}

func subscriptionName(sub rocket.Subscription) string {
	if name := strings.TrimSpace(sub.FriendlyName); name != "" {
		return name
	}
	if name := strings.TrimSpace(sub.Name); name != "" {
		return name
	}
	return strings.TrimSpace(sub.RoomID)
}

func messageAuthor(message rocket.Message) string {
	if username := strings.TrimPrefix(strings.TrimSpace(message.User.Username), "@"); username != "" {
		return "@" + username
	}
	if name := strings.TrimSpace(message.User.Name); name != "" {
		return name
	}
	if id := strings.TrimSpace(message.User.ID); id != "" {
		return id
	}
	return "unknown"
}

func renderMessage(message rocket.Message) string {
	if text := strings.TrimSpace(message.Text); text != "" {
		return text
	}
	if message.DecryptError != "" {
		return "[decryption failed: " + message.DecryptError + "]"
	}
	if message.Encrypted && !message.Decrypted {
		return "[encrypted message]"
	}
	if message.File != nil || len(message.Files) > 0 || len(message.Attachments) > 0 {
		return "[file or attachment]"
	}
	return "[empty message]"
}
