# RocketGo

RocketGo is a [Go](https://golang.org/) package that provides bindings to the
[Rocket.Chat](https://rocket.chat/) API. It covers both REST and realtime (DDP
over websocket), handles session persistence, and optionally does E2EE so your
bot can talk in encrypted rooms without any extra work.

## Getting Started

### Installing

This assumes you already have a working Go environment, if not please see
[this page](https://golang.org/doc/install) first.

```sh
go get github.com/thrillhunter21/rocketgo
```

### Usage

Import the package into your project.

```go
import rocket "github.com/thrillhunter21/rocketgo"
```

Create a client and connect:

```go
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

if err := client.Connect(context.Background()); err != nil {
    panic(err)
}
if err := client.WatchJoinedRooms(context.Background()); err != nil {
    panic(err)
}
```

See Documentation and Examples below for more.

## Documentation

The RocketGo code is documented with Go doc comments. Go reference presents
that in a nice format.

Good places to start:

- `New` / `Config`
- `Client.Connect`
- `Client.WatchJoinedRooms`
- `Client.Events`
- `ParseCommand`

## Examples

- [examples/say](https://github.com/thrillhunter21/rocketgo/tree/main/examples/say) - a `;say hello world` echo bot
- [examples/history](https://github.com/thrillhunter21/rocketgo/tree/main/examples/history) - list joined rooms or print room history

Both have their config at the top of the file so you can fill it in and run:

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

`E2EE.Password` is what encrypts/decrypts the user's private key on the server.

If the account has never used E2EE before, this can be any string you want --
the client will generate a keypair and protect it with that value. If the
account already has E2EE set up (e.g. through the Rocket.Chat web UI), use
whatever password or recovery phrase was originally set, including the 12-word
mnemonic if that's what it was.

Once connected, encrypted rooms just work -- outgoing messages and file uploads
get encrypted, incoming ones get decrypted. The keypair and session tokens are
cached under `.rocket/` so you don't re-derive everything on every restart.

## Notes

- `WatchJoinedRooms()` subscribes to every room the user is in. Easiest way to
  get going.
- `ParseCommand()` / `Message.ParseCommand()` parse things like `;say hello`
  locally, which is handy in encrypted rooms where slash commands don't work.
- Logger is optional, defaults to `slog.Default()`.
- Downloads are restricted to the same origin by default. If you need external
  hosts, set `AllowedDownloadHosts`.
