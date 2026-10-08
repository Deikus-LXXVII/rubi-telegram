# Unofficial Telegram for Rubi

A [Rubi](https://github.com/Deikus-LXXVII/rubi) plugin that lets your AI agent work with Telegram, with
your approval. It is an unofficial Telegram client, built on the Telegram API; it is not made or endorsed
by Telegram. You can connect a bot, your personal account, or several of each:

- see your recent chats with unread counts, and read messages (never marks anything as read);
- search your messages (personal accounts);
- save a draft in a chat, where you'll find it in your Telegram apps, after you approve it (personal accounts);
- send messages only after you approve each one in the Rubi panel (with your passkey by default), and
  optionally get notified when someone replies;
- let your agent watch for new messages from given chats or people, or with given words, and react to
  each one; a bot can also wake your agent for every message it receives;
- keep private messages private: Telegram's own login codes always, other sign-in codes, password resets
  and anything you choose stay hidden from your agent unless you approve showing one;
- choose which chats your agent can see, and whether it may ask for the others.

## Install

Ask your agent: "Connect my Telegram through Rubi." It installs the plugin from the Rubi store (you
approve what it can do), then sends you a setup link. There you choose what to connect.

### A bot

1. In Telegram, open [@BotFather](https://t.me/BotFather) and send `/newbot` (or `/mybots` to pick an
   existing bot, then *API Token*).
2. Paste the token into Rubi. It shows up as "@yourbot (bot)".

A bot can only write to people who started it and to groups it was added to, and it only sees what is
sent to it after Rubi connects it (a bot can't read history). In groups, a bot with privacy mode on (the
default) only receives commands and mentions. If another program already uses the bot (or it has a
webhook), Rubi can't receive its messages; give Rubi its own bot.

### Your personal account

Rubi logs in as you, like a Telegram app does. You need your own API keys:

1. Open [my.telegram.org](https://my.telegram.org) and log in with your phone number.
2. Choose *API development tools* and create an app: any title (for example "Rubi"), platform "Other".
3. Copy **api_id** and **api_hash** into Rubi, with your phone number.
4. Telegram sends you a login code (in your Telegram app, or by SMS). Enter it in Rubi, never anywhere
   else, and never give it to anyone, including your agent.
5. If your account has two-step verification, Rubi asks for your Telegram cloud password. It is used once
   to log in and isn't stored.

There is one api_id / api_hash pair per phone number: don't share them. The account shows up in Telegram
under *Settings > Devices* as "Rubi"; ending that session there disconnects Rubi. Rubi keeps the
api_hash and the login session encrypted; your agent never sees them.

To add another account, open *Settings* in the Rubi panel and press *Add account* at Telegram.

## Please read: Telegram's rules and the risk to your account

Connecting your **personal account** uses Telegram's API the way unofficial Telegram apps do. Be aware:

- **Ban risk.** Telegram watches accounts that log in through unofficial clients. Accounts that behave like
  spam bots (many messages, mass joins, messaging strangers) can be limited or banned, sometimes without
  warning, and new accounts are watched most closely. Rubi acts slowly (it spaces out its requests and
  waits when Telegram asks it to), never does anything in bulk, and sends nothing without your approval.
  It still can't promise that Telegram won't restrict an account. If losing your account would hurt,
  connect a bot instead.
- **Telegram's API Terms of Service** ([core.telegram.org/api/terms](https://core.telegram.org/api/terms))
  say, among other things, that an app must not act for users without their knowledge and consent, and
  that Telegram data must not be used to train, fine-tune or deploy AI or machine-learning models. With
  this plugin an AI agent reads your messages to help you, at your request, which arguably falls under
  that last rule. Decide for yourself whether you're comfortable with that; this plugin doesn't train
  anything, but it does show your messages to the AI model behind your agent.
- **Read receipts.** The plugin never marks messages as read: your contacts see that you read a message
  when you read it in your own Telegram app. (Telegram's terms also forbid "ghost modes"; the plugin
  doesn't alter read receipts, it just leaves them to you.)
- **Bots** are much safer: they are made to be automated, and the Bot API is used as intended.

## Permissions

| | |
|---|---|
| Asks for | for a bot: its token; for your account: api_id, api_hash, phone number, login code, two-step verification password (not stored) |
| Read chats and messages | no approval by default |
| Save a draft in a chat (personal accounts; may replace your unsent text) | buttons in the agent chat by default |
| Send a Telegram message | passkey / password by default |
| Watch for new messages (wakes your agent) | no approval by default |
| Show a private message | passkey / password, always |
| Open a closed chat for a while | passkey / password, always |
| Connects to | `api.telegram.org:443` (bots); Telegram's data centers `149.154.160.0/20` and `91.108.4.0/22`, port 443 (personal accounts) |
| Can notify your agent about | new messages matching its watches, messages to a bot, replies to tracked messages |

You can change the approval levels in Rubi's settings, except the two marked "always".

## Your settings

Right after connecting, and later in the Rubi panel under *Settings*, at each Telegram account:

- **Chats your agent can see** (for each account): all, or only the ones you check; and whether it may
  ask for the others. Access you approve lasts an hour or a day.
- **Hide sign-in codes, one-time passwords and confirmation links:** on by default. Messages from Telegram
  itself (the 777000 account that sends your Telegram login codes), and messages forwarded from it, are
  always hidden, whatever you choose, and can never be shown to your agent, not even with your approval.
- **Hide password reset messages:** on by default.
- **Hide sign-in and security alerts:** off by default, so your agent can warn you about new sign-ins or
  suspicious activity. (An alert that contains a code stays hidden by the first switch.)
- **Hidden chats** and **hidden words:** anything else you want to keep from your agent.
- **Wake my agent for every message to a bot:** on by default.

The privacy settings apply to all your Telegram accounts. Only you can change these settings, with your
passkey or password; your agent can't see or change them. Hidden messages show up to your agent only as
"a private message from <sender>".

## Development

```bash
go build -o telegram . && ./telegram --manifest
go test -race ./...
```

The tests use fakes only: an `httptest` server for the Bot API and in-memory stand-ins for the MTProto
client (`userClient`, `userLogin` in `types.go`). They never contact Telegram. The MTProto side
(`gotd.go`) uses [gotd/td](https://github.com/gotd/td).

To develop against a local checkout of Rubi, use a workspace (not committed):

```bash
go work init . ../rubi
```

## Releasing

Tag `vX.Y.Z`. The release workflow tests, builds for linux/amd64, linux/arm64 and darwin/arm64, writes
`rubi-plugin.json`, signs `SHA256SUMS` with the plugin key (`PLUGIN_SIGNING_KEY`; public half in
`publisher-key.pub.pem`), attests the build, and publishes the release. It prints the SHA-256 of
`SHA256SUMS` for the Rubi store catalog.

## License

[MIT](LICENSE)
