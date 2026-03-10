# RocketGo

RocketGo is a Go package for Rocket.Chat.

It provides a stateful client with REST helpers, DDP websocket events, session persistence, and end-to-end encryption support. The goal is to behave like a real Rocket.Chat client rather than a thin REST wrapper.

## Features

- Login with username/email + password, or `UserID` + `AuthToken`
- DDP event stream for messages, reactions, typing, room changes, subscription changes, notifications, and deletes
- Send, edit, reply, delete, react, pin, star, mark read, and typing helpers
- Room and subscription caches with lookup helpers
- File upload and download support, including encrypted attachments
- E2EE identity, room key, and key request handling
- Session and E2EE persistence under `.rocket/`
- Safe downloads with same-origin enforcement and optional host allowlist

## Getting Started

### Installing

For local use from this folder:

```sh
go mod tidy
```

Install the latest version with:

```sh
go get github.com/thrillhunter21/rocketgo
```

### Usage

```go
package main

import (
	"context"
	"log/slog"
	"os"

	rocket "github.com/thrillhunter21/rocketgo"
)

func main() {
	client, err := rocket.New(rocket.Config{
		ServerURL:      "https://chat.example.com",
		Login:          "bot-user",
		Password:       "bot-password",
		DataDir:        ".rocket",
		PersistSession: true,
		AutoReconnect:  true,
		Logger:         slog.New(slog.NewTextHandler(os.Stdout, nil)),
		E2EE: rocket.E2EEConfig{
			Enabled:  true,
			Password: "e2ee-password",
		},
	})
	if err != nil {
		panic(err)
	}
	defer client.Close()

	if err := client.Connect(context.Background()); err != nil {
		panic(err)
	}
	if err := client.WatchJoinedRooms(context.Background()); err != nil {
		panic(err)
	}
}
```

See the examples below for complete runnable programs.

## Documentation

The exported API is documented in code and is intended to be straightforward to explore with Go reference tooling.

Useful starting points:

- `New`
- `Client.Connect`
- `Client.WatchJoinedRooms`
- `Client.Events`
- `ParseCommand`

## Examples

- `examples/say`
  - Watches joined rooms and replies to `;say hello world`
- `examples/history`
  - Lists joined rooms or loads room history for a chosen room

Run them with:

```sh
go run ./examples/say
go run ./examples/history
```

Both examples keep their config inline at the top of the file so you can edit and run them quickly.

## E2EE

Enable E2EE with:

```go
E2EE: rocket.E2EEConfig{
	Enabled:  true,
	Password: "e2ee-password",
}
```

`E2EE.Password` is the secret used to encrypt and decrypt the user's private key.

For a **new account** that has never used E2EE, this can be any stable string you choose. The client will generate a key pair and protect it with this value.

For an **existing account** that already has E2EE set up, set this to the exact password or recovery phrase that account was given by Rocket.Chat (e.g. the 12-word mnemonic).

No separate import or recovery step is needed. The client fetches the encrypted private key from the server and unlocks it with `E2EE.Password`. The identity is also cached locally under `.rocket/`.

Outgoing text and file uploads in encrypted rooms are encrypted automatically, and incoming messages and attachments are decrypted when the required room keys are available.

## Notes

- `WatchJoinedRooms()` is the simplest way to start receiving room message events.
- `ParseCommand()` and `Message.ParseCommand()` are useful for local commands like `;say` inside encrypted rooms.
- The logger is optional. If omitted, the package uses `slog.Default()`.
- If your files are served from a trusted external host, set `AllowedDownloadHosts`.

## Status

This package is a good fit for an initial public `v0.x` release.
