package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	rocket "github.com/thrillhunter/rocketgo"
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
		Password: "e2ee-password", // any stable string for new accounts, or the existing recovery phrase
	},
}

const commandPrefix = ";"

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	config.Logger = slog.Default()

	client, err := rocket.New(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create client: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client.AddHandler(func(c *rocket.Client, event *rocket.MessageEvent) {
		if err := handleMessage(ctx, c, event.Message); err != nil {
			slog.Error("handle message failed", "error", err)
		}
	})

	if err := client.Connect(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "connect failed: %v\n", err)
		os.Exit(1)
	}
	if err := client.WatchJoinedRooms(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "watch setup failed: %v\n", err)
		os.Exit(1)
	}

	slog.Info("say example connected", "user_id", client.Session().UserID)

	select {
	case <-ctx.Done():
	case <-client.Done():
	}
}

func handleMessage(ctx context.Context, client *rocket.Client, message *rocket.Message) error {
	if message.User.ID == client.Session().UserID {
		return nil
	}

	command, ok := message.ParseCommand(commandPrefix)
	if !ok || command.Name != "say" {
		return nil
	}

	reply := strings.TrimSpace(command.RawArgs)
	if reply == "" {
		reply = "Usage: `;say hello world`"
	}

	target := replyTarget(message)
	if target == "" {
		_, err := client.SendText(ctx, message.RoomID, reply)
		return err
	}

	_, err := client.ReplyText(ctx, message.RoomID, target, reply)
	return err
}

func replyTarget(message *rocket.Message) string {
	if threadID := strings.TrimSpace(message.ThreadMessageID); threadID != "" {
		return threadID
	}
	return strings.TrimSpace(message.ID)
}
