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

Import the package into your project.

```go
import rocket "github.com/thrillhunter/rocketgo"
```

Create a client, register handlers, then connect:

```go
ctx := context.Background()

client, err := rocket.New(rocket.Config{
	ServerURL:      "https://chat.example.com",
	Login:          "bot-user",
	Password:       "bot-password",
	DataDir:        ".rocket",
	PersistSession: true,
	AutoReconnect:  true,
	E2EE: rocket.E2EEConfig{
		Enabled:  true,
		Password: "e2ee-password",
	},
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

Handlers run concurrently by default. Set `client.SyncEvents = true` if you
want inline dispatch instead.

## Documentation

Full reference on [pkg.go.dev](https://pkg.go.dev/github.com/thrillhunter/rocketgo).

Register typed handlers with `AddHandler` or `AddHandlerOnce`. Use
`func(c *rocket.Client, interface{})` as the signature to receive every event.

## Examples

- [examples/say](https://github.com/thrillhunter/rocketgo/tree/main/examples/say) - `;say hello world` echo bot
- [examples/history](https://github.com/thrillhunter/rocketgo/tree/main/examples/history) - list joined rooms or print room history

Run them with:

```sh
go run ./examples/say
go run ./examples/history
```

## E2EE

Set `E2EE.Enabled` and give it a password:

```go
E2EE: rocket.E2EEConfig{
	Enabled:  true,
	Password: "e2ee-password",
}
```

`E2EE.Password` is what encrypts and decrypts the user's private key.

For a new account, this can be any stable string you choose. The client will
generate a keypair and protect it with that value.

For an existing account, use the password or recovery phrase that account
already uses for Rocket.Chat E2EE, including the 12-word mnemonic if that is
what the server gave you.

Once connected, encrypted rooms work like normal rooms. Outgoing messages and
file uploads are encrypted, incoming ones are decrypted, and the local keypair
and session tokens are cached under `.rocket/`.

## Notes

- `WatchJoinedRooms()` subscribes to every room the user is in.
- `ParseCommand()` / `Message.ParseCommand()` are useful for local commands in encrypted rooms.
- Logger is optional and defaults to `slog.Default()`.
- Downloads are restricted to the same origin by default. If you need external hosts, set `AllowedDownloadHosts`.
