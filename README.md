# RocketGo

RocketGo is a [Go](https://golang.org/) package for [Rocket.Chat](https://rocket.chat/).
It wraps the REST API and the realtime DDP websocket, keeps session state on
disk when you want it to, and can handle E2EE so bots can work in encrypted
rooms.

## Getting Started

### Installing

```sh
go get github.com/thrillhunter/rocketgo
```

### Usage

```go
import rocket "github.com/thrillhunter/rocketgo"
```

Create a client, register handlers, and connect:

```go
ctx := context.Background()

client, err := rocket.New(rocket.Config{
	ServerURL:      "https://chat.example.com",
	Login:          "bot-user",
	Password:       "bot-password",
	DataDir:        ".rocket",
	PersistSession: true,
	AutoReconnect:  true,
})
if err != nil {
	panic(err)
}
defer client.Close()

client.AddHandler(func(c *rocket.Client, event *rocket.MessageEvent) {
	fmt.Println(event.Message.Text)
})

if err := client.Connect(ctx); err != nil {
	panic(err)
}
if err := client.WatchJoinedRooms(ctx); err != nil {
	panic(err)
}
```

Each handler runs in its own goroutine. Use `AddHandler` for persistent
handlers and `AddHandlerOnce` for one-shot handlers. Pass
`func(c *rocket.Client, interface{})` as the signature to receive every event
regardless of type.

Full API reference on [pkg.go.dev](https://pkg.go.dev/github.com/thrillhunter/rocketgo).

## Examples

- [examples/say](https://github.com/thrillhunter/rocketgo/tree/main/examples/say) — echo bot that responds to `;say hello world`
- [examples/history](https://github.com/thrillhunter/rocketgo/tree/main/examples/history) — list joined rooms or print room history

```sh
go run ./examples/say
go run ./examples/history
```

## E2EE

Enable end-to-end encryption by setting `E2EE.Enabled` and providing a
password:

```go
client, err := rocket.New(rocket.Config{
	ServerURL: "https://chat.example.com",
	Login:     "bot-user",
	Password:  "bot-password",
	E2EE: rocket.E2EEConfig{
		Enabled:  true,
		Password: "e2ee-password",
	},
})
```

The password encrypts and decrypts the user's private key. For a new account
this can be any stable string you choose — the client will generate a keypair
and protect it with that value. For an existing account, use the password or
recovery phrase already associated with that account's E2EE setup, including
the 12-word mnemonic if that is what the server assigned.

Once connected, encrypted rooms work transparently. Outgoing messages and file
uploads are encrypted, incoming ones are decrypted, and the keypair and session
tokens are cached under `DataDir` (defaults to `.rocket/`).

## Notes

- `WatchJoinedRooms()` subscribes to every room the user belongs to.
- `ParseCommand()` / `Message.ParseCommand()` handle prefixed commands, useful in encrypted rooms where server-side slash commands don't work.
- Logger is optional and defaults to `slog.Default()`.
- Downloads are restricted to the same origin. Set `AllowedDownloadHosts` if you need external hosts.
