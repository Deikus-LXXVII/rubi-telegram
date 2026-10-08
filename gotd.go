package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// The personal-account client, on gotd (MTProto). Conservative by design: every call goes through a
// pacer (at most one request per callPace per account) and waits out short FLOOD_WAITs; nothing is done
// in bulk; nothing is marked as read (messages.readHistory is never called).

var (
	callPace     = 700 * time.Millisecond
	maxFloodWait = 60 * time.Second
)

var device = telegram.DeviceConfig{DeviceModel: "Rubi", SystemVersion: "Rubi plugin", AppVersion: "1.0", LangCode: "en"}

// memSession is a session.Storage in memory; the plugin saves its content in the account's secret.
type memSession struct {
	mu      sync.Mutex
	data    []byte
	onStore func(data []byte) // optional: told about every new session
}

func (s *memSession) LoadSession(context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.data) == 0 {
		return nil, session.ErrNotFound
	}
	return append([]byte(nil), s.data...), nil
}

func (s *memSession) StoreSession(_ context.Context, data []byte) error {
	s.mu.Lock()
	changed := string(s.data) != string(data)
	s.data = append([]byte(nil), data...)
	fn := s.onStore
	s.mu.Unlock()
	if changed && fn != nil {
		fn(append([]byte(nil), data...))
	}
	return nil
}

// pacer spaces requests out and waits out short flood waits.
type pacer struct {
	mu   sync.Mutex
	next time.Time
}

func (p *pacer) wait(ctx context.Context) error {
	p.mu.Lock()
	now := time.Now()
	at := p.next
	if at.Before(now) {
		at = now
	}
	p.next = at.Add(callPace)
	p.mu.Unlock()
	if d := time.Until(at); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	return nil
}

func (p *pacer) middleware() telegram.Middleware {
	return telegram.MiddlewareFunc(func(next tg.Invoker) telegram.InvokeFunc {
		return func(ctx context.Context, in bin.Encoder, out bin.Decoder) error {
			for attempt := 0; ; attempt++ {
				if err := p.wait(ctx); err != nil {
					return err
				}
				err := next.Invoke(ctx, in, out)
				d, ok := tgerr.AsFloodWait(err)
				if !ok || attempt >= 2 || d > maxFloodWait {
					return err
				}
				p.mu.Lock() // everyone waits
				if until := time.Now().Add(d + time.Second); until.After(p.next) {
					p.next = until
				}
				p.mu.Unlock()
			}
		}
	})
}

// runner runs a gotd client in the background until closed.
type runner struct {
	client *telegram.Client
	cancel context.CancelFunc
	done   chan struct{}
	err    error
	once   sync.Once
}

// start runs the client; work runs inside the connection (after ready is signalled) until it returns.
func start(ctx context.Context, client *telegram.Client, work func(ctx context.Context, ready func(error)) error) (*runner, error) {
	rctx, cancel := context.WithCancel(context.Background())
	r := &runner{client: client, cancel: cancel, done: make(chan struct{})}
	readyCh := make(chan error, 1)
	var readyOnce sync.Once
	ready := func(err error) { readyOnce.Do(func() { readyCh <- err }) }
	go func() {
		defer close(r.done)
		r.err = client.Run(rctx, func(ctx context.Context) error { return work(ctx, ready) })
		if r.err == nil {
			r.err = errors.New("disconnected")
		}
		if auth.IsUnauthorized(r.err) || auth.IsKeyUnregistered(r.err) || tgerr.Is(r.err, "SESSION_REVOKED", "USER_DEACTIVATED", "USER_DEACTIVATED_BAN") {
			r.err = fmt.Errorf("%w (%v)", errSessionRevoked, r.err)
		}
		ready(r.err)
	}()
	select {
	case err := <-readyCh:
		if err != nil {
			r.Close()
			return nil, err
		}
		return r, nil
	case <-ctx.Done():
		r.Close()
		return nil, ctx.Err()
	}
}

func (r *runner) Close() {
	r.once.Do(func() {
		r.cancel()
		<-r.done
	})
}

func newClient(cfg userConfig, store *memSession, handler telegram.UpdateHandler) *telegram.Client {
	p := &pacer{}
	return telegram.NewClient(cfg.APIID, cfg.APIHash, telegram.Options{
		SessionStorage: store,
		UpdateHandler:  handler,
		NoUpdates:      handler == nil,
		Middlewares:    []telegram.Middleware{p.middleware()},
		Device:         device,
	})
}

// ---- login ----

type gotdLoginT struct {
	*runner
	store *memSession
}

func gotdLogin(ctx context.Context, cfg userConfig) (userLogin, error) {
	store := &memSession{}
	r, err := start(ctx, newClient(cfg, store, nil), func(ctx context.Context, ready func(error)) error {
		ready(nil)
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil {
		return nil, err
	}
	return &gotdLoginT{runner: r, store: store}, nil
}

func (l *gotdLoginT) SendCode(ctx context.Context, phone string) (string, string, error) {
	sent, err := l.client.Auth().SendCode(ctx, phone, auth.SendCodeOptions{})
	if err != nil {
		switch {
		case tgerr.Is(err, "PHONE_NUMBER_INVALID"):
			return "", "", errors.New("Telegram says the phone number is invalid; include the country code")
		case tgerr.Is(err, "API_ID_INVALID", "API_ID_PUBLISHED_FLOOD"):
			return "", "", errors.New("Telegram rejected api_id / api_hash; copy both again from my.telegram.org")
		case tgerr.Is(err, "PHONE_NUMBER_BANNED"):
			return "", "", errors.New("Telegram says this phone number is banned")
		}
		if d, ok := tgerr.AsFloodWait(err); ok {
			return "", "", fmt.Errorf("Telegram asks to wait %s before sending another code", d.Round(time.Second))
		}
		return "", "", err
	}
	code, ok := sent.(*tg.AuthSentCode)
	if !ok {
		return "", "", errors.New("Telegram answered the code request unexpectedly; try again later")
	}
	via := ""
	switch code.Type.(type) {
	case *tg.AuthSentCodeTypeApp:
		via = "to your Telegram app (a message from \"Telegram\")"
	case *tg.AuthSentCodeTypeSMS:
		via = "by SMS"
	case *tg.AuthSentCodeTypeCall:
		via = "by a phone call"
	case *tg.AuthSentCodeTypeEmailCode:
		via = "to your login email"
	}
	return code.PhoneCodeHash, via, nil
}

func selfOf(a *tg.AuthAuthorization) (selfInfo, error) {
	u, ok := a.User.(*tg.User)
	if !ok {
		return selfInfo{}, errors.New("Telegram returned no user")
	}
	return selfInfo{ID: u.ID, Username: u.Username, Name: strings.TrimSpace(u.FirstName + " " + u.LastName), Phone: u.Phone}, nil
}

func (l *gotdLoginT) SignIn(ctx context.Context, phone, code, hash string) (selfInfo, error) {
	a, err := l.client.Auth().SignIn(ctx, phone, code, hash)
	var signUp *auth.SignUpRequired
	switch {
	case errors.Is(err, auth.ErrPasswordAuthNeeded):
		return selfInfo{}, errNeedPassword
	case tgerr.Is(err, "PHONE_CODE_INVALID", "PHONE_CODE_EMPTY"):
		return selfInfo{}, errBadCode
	case tgerr.Is(err, "PHONE_CODE_EXPIRED"):
		return selfInfo{}, errCodeExpired
	case errors.As(err, &signUp):
		return selfInfo{}, errNoAccount
	case err != nil:
		return selfInfo{}, err
	}
	return selfOf(a)
}

func (l *gotdLoginT) Password(ctx context.Context, password string) (selfInfo, error) {
	a, err := l.client.Auth().Password(ctx, password)
	if errors.Is(err, auth.ErrPasswordInvalid) {
		return selfInfo{}, errBadPassword
	}
	if err != nil {
		return selfInfo{}, err
	}
	return selfOf(a)
}

func (l *gotdLoginT) Session() (string, error) {
	data, err := l.store.LoadSession(context.Background())
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// ---- connected account ----

type gotdUser struct {
	*runner
	api    *tg.Client
	gaps   *updates.Manager
	selfID int64

	mu    sync.Mutex
	peers map[int64]tg.InputPeerClass // marked id → how to address it
	chats map[int64]chatInfo
	names map[int64]userName // user id → name
}

type userName struct{ name, username string }

func gotdConnect(ctx context.Context, cfg userConfig, onMessage func(message)) (userClient, error) {
	raw, err := base64.StdEncoding.DecodeString(cfg.Session)
	if err != nil || len(raw) == 0 {
		return nil, errSessionRevoked
	}
	store := &memSession{data: raw}
	if cfg.OnSession != nil {
		store.onStore = func(data []byte) { cfg.OnSession(base64.StdEncoding.EncodeToString(data)) }
	}
	u := &gotdUser{peers: map[int64]tg.InputPeerClass{}, chats: map[int64]chatInfo{}, names: map[int64]userName{}}
	d := tg.NewUpdateDispatcher()
	handle := func(e tg.Entities, msg tg.MessageClass) {
		u.learnEntities(e)
		m, ok := u.convert(msg)
		if ok && !m.Out && onMessage != nil {
			onMessage(m)
		}
	}
	d.OnNewMessage(func(ctx context.Context, e tg.Entities, up *tg.UpdateNewMessage) error {
		handle(e, up.Message)
		return nil
	})
	d.OnNewChannelMessage(func(ctx context.Context, e tg.Entities, up *tg.UpdateNewChannelMessage) error {
		handle(e, up.Message)
		return nil
	})
	u.gaps = updates.New(updates.Config{Handler: d, MaxChannelDifferenceConcurrency: 1})
	client := newClient(cfg, store, u.gaps)
	u.api = client.API()
	r, err := start(ctx, client, func(ctx context.Context, ready func(error)) error {
		self, err := client.Self(ctx)
		if err != nil {
			ready(err)
			return err
		}
		u.selfID = self.ID
		u.learnUsers([]tg.UserClass{self})
		ready(nil)
		return u.gaps.Run(ctx, u.api, self.ID, updates.AuthOptions{})
	})
	if err != nil {
		return nil, err
	}
	u.runner = r
	return u, nil
}

func (u *gotdUser) Done() <-chan struct{} { return u.done }
func (u *gotdUser) Err() error {
	select {
	case <-u.done:
		return u.err
	default:
		return nil
	}
}

// Marked ids, as in the Bot API.
const channelBase = -1000000000000

func markUser(id int64) int64    { return id }
func markChat(id int64) int64    { return -id }
func markChannel(id int64) int64 { return channelBase - id }

func markPeer(p tg.PeerClass) int64 {
	switch p := p.(type) {
	case *tg.PeerUser:
		return markUser(p.UserID)
	case *tg.PeerChat:
		return markChat(p.ChatID)
	case *tg.PeerChannel:
		return markChannel(p.ChannelID)
	}
	return 0
}

func (u *gotdUser) learnUsers(list []tg.UserClass) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, c := range list {
		x, ok := c.(*tg.User)
		if !ok {
			continue
		}
		name := strings.TrimSpace(x.FirstName + " " + x.LastName)
		if x.Deleted && name == "" {
			name = "Deleted account"
		}
		u.names[x.ID] = userName{name: name, username: x.Username}
		typ := "private"
		if x.Bot {
			typ = "bot"
		}
		title := name
		if x.Self {
			title = "Saved Messages"
			u.peers[x.ID] = &tg.InputPeerSelf{}
		} else if _, known := u.peers[x.ID]; !known || !x.Min {
			u.peers[x.ID] = &tg.InputPeerUser{UserID: x.ID, AccessHash: x.AccessHash}
		}
		c := u.chats[x.ID]
		c.ID, c.Title, c.Type, c.Username = x.ID, title, typ, x.Username
		u.chats[x.ID] = c
	}
}

func (u *gotdUser) learnChats(list []tg.ChatClass) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, c := range list {
		switch x := c.(type) {
		case *tg.Chat:
			id := markChat(x.ID)
			u.peers[id] = &tg.InputPeerChat{ChatID: x.ID}
			ci := u.chats[id]
			ci.ID, ci.Title, ci.Type = id, x.Title, "group"
			u.chats[id] = ci
		case *tg.Channel:
			id := markChannel(x.ID)
			if _, known := u.peers[id]; !known || !x.Min {
				u.peers[id] = &tg.InputPeerChannel{ChannelID: x.ID, AccessHash: x.AccessHash}
			}
			typ := "supergroup"
			if x.Broadcast {
				typ = "channel"
			}
			ci := u.chats[id]
			ci.ID, ci.Title, ci.Type, ci.Username = id, x.Title, typ, x.Username
			u.chats[id] = ci
		case *tg.ChannelForbidden:
			id := markChannel(x.ID)
			ci := u.chats[id]
			ci.ID, ci.Title, ci.Type = id, x.Title, "channel"
			u.chats[id] = ci
		}
	}
}

func (u *gotdUser) learnEntities(e tg.Entities) {
	var users []tg.UserClass
	for _, x := range e.Users {
		users = append(users, x)
	}
	var chats []tg.ChatClass
	for _, x := range e.Chats {
		chats = append(chats, x)
	}
	for _, x := range e.Channels {
		chats = append(chats, x)
	}
	u.learnUsers(users)
	u.learnChats(chats)
}

func (u *gotdUser) learn(r interface {
	GetUsers() []tg.UserClass
	GetChats() []tg.ChatClass
}) {
	u.learnUsers(r.GetUsers())
	u.learnChats(r.GetChats())
}

func mediaKind(m tg.MessageMediaClass) string {
	switch m.(type) {
	case nil, *tg.MessageMediaEmpty:
		return ""
	case *tg.MessageMediaPhoto:
		return "photo"
	case *tg.MessageMediaDocument:
		return "document"
	case *tg.MessageMediaGeo, *tg.MessageMediaGeoLive, *tg.MessageMediaVenue:
		return "location"
	case *tg.MessageMediaContact:
		return "contact"
	case *tg.MessageMediaPoll:
		return "poll"
	case *tg.MessageMediaWebPage:
		return "link preview"
	}
	return "media"
}

// convert turns a gotd message into the plugin's message.
func (u *gotdUser) convert(mc tg.MessageClass) (message, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	var m message
	var from tg.PeerClass
	switch x := mc.(type) {
	case *tg.Message:
		m = message{ID: x.ID, ChatID: markPeer(x.PeerID), Out: x.Out, Date: time.Unix(int64(x.Date), 0).UTC(), Text: x.Message,
			Media: mediaKind(x.Media)}
		if h, ok := x.ReplyTo.(*tg.MessageReplyHeader); ok {
			m.ReplyTo = h.ReplyToMsgID
		}
		from, _ = x.GetFromID()
		if fwd, ok := x.GetFwdFrom(); ok {
			if p, ok := fwd.GetFromID(); ok {
				m.FwdFromID = markPeer(p)
			} else if p, ok := fwd.GetSavedFromPeer(); ok {
				m.FwdFromID = markPeer(p)
			}
		}
	case *tg.MessageService:
		m = message{ID: x.ID, ChatID: markPeer(x.PeerID), Out: x.Out, Date: time.Unix(int64(x.Date), 0).UTC(),
			Text: "[service: " + strings.TrimPrefix(x.Action.TypeName(), "messageAction") + "]"}
		from, _ = x.GetFromID()
	default:
		return message{}, false
	}
	if c, ok := u.chats[m.ChatID]; ok {
		m.ChatTitle, m.ChatType = c.Title, c.Type
	}
	switch {
	case from != nil:
		m.FromID = markPeer(from)
	case m.Out:
		m.FromID = u.selfID
	case m.ChatID > 0:
		m.FromID = m.ChatID // a one-to-one chat: the other side
	}
	if m.FromID > 0 {
		n := u.names[m.FromID]
		m.From, m.FromUsername = n.name, n.username
	} else if c, ok := u.chats[m.FromID]; ok && m.FromID != 0 {
		m.From, m.FromUsername = c.Title, c.Username
	}
	return m, true
}

func (u *gotdUser) messagesOf(res tg.MessagesMessagesClass) []message {
	mod, ok := res.AsModified()
	if !ok {
		return nil
	}
	u.learn(mod)
	var out []message
	for _, mc := range mod.GetMessages() {
		if m, ok := u.convert(mc); ok {
			out = append(out, m)
		}
	}
	return out
}

func (u *gotdUser) peer(ctx context.Context, chatID int64) (tg.InputPeerClass, error) {
	u.mu.Lock()
	p, ok := u.peers[chatID]
	u.mu.Unlock()
	if ok {
		return p, nil
	}
	// Not seen yet: the dialogs list brings most chats with their access hashes.
	if _, err := u.Dialogs(ctx, 100); err != nil {
		return nil, err
	}
	u.mu.Lock()
	p, ok = u.peers[chatID]
	u.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown chat %d; use an id from %s or an @username", chatID, tool("chats"))
	}
	return p, nil
}

func (u *gotdUser) Dialogs(ctx context.Context, limit int) ([]chatInfo, error) {
	res, err := u.api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{OffsetPeer: &tg.InputPeerEmpty{}, Limit: limit})
	if err != nil {
		return nil, err
	}
	mod, ok := res.AsModified()
	if !ok {
		return nil, errors.New("Telegram returned no dialogs")
	}
	u.learn(mod)
	last := map[int64]time.Time{}
	for _, mc := range mod.GetMessages() {
		if x, ok := mc.(*tg.Message); ok {
			last[markPeer(x.PeerID)] = time.Unix(int64(x.Date), 0).UTC()
		} else if x, ok := mc.(*tg.MessageService); ok {
			last[markPeer(x.PeerID)] = time.Unix(int64(x.Date), 0).UTC()
		}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []chatInfo
	for _, dc := range mod.GetDialogs() {
		d, ok := dc.(*tg.Dialog)
		if !ok {
			continue
		}
		id := markPeer(d.Peer)
		c := u.chats[id]
		c.ID, c.Unread, c.LastDate = id, d.UnreadCount, last[id]
		if c.Title == "" {
			c.Title = strconv.FormatInt(id, 10)
		}
		out = append(out, c)
	}
	return out, nil
}

func (u *gotdUser) History(ctx context.Context, chatID int64, limit, beforeID int) ([]message, error) {
	p, err := u.peer(ctx, chatID)
	if err != nil {
		return nil, err
	}
	res, err := u.api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: p, OffsetID: beforeID, Limit: limit})
	if err != nil {
		return nil, err
	}
	return u.messagesOf(res), nil
}

func (u *gotdUser) Messages(ctx context.Context, chatID int64, ids []int) ([]message, error) {
	in := make([]tg.InputMessageClass, len(ids))
	for i, id := range ids {
		in[i] = &tg.InputMessageID{ID: id}
	}
	p, err := u.peer(ctx, chatID)
	if err != nil {
		return nil, err
	}
	var res tg.MessagesMessagesClass
	if ch, ok := p.(*tg.InputPeerChannel); ok {
		res, err = u.api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: ch.ChannelID, AccessHash: ch.AccessHash}, ID: in})
	} else {
		res, err = u.api.MessagesGetMessages(ctx, in)
	}
	if err != nil {
		return nil, err
	}
	var out []message
	for _, m := range u.messagesOf(res) {
		if m.ChatID == chatID {
			out = append(out, m)
		}
	}
	return out, nil
}

func (u *gotdUser) Search(ctx context.Context, query string, chatID int64, limit int) ([]message, error) {
	var res tg.MessagesMessagesClass
	var err error
	if chatID == 0 {
		res, err = u.api.MessagesSearchGlobal(ctx, &tg.MessagesSearchGlobalRequest{Q: query, Filter: &tg.InputMessagesFilterEmpty{},
			OffsetPeer: &tg.InputPeerEmpty{}, Limit: limit})
	} else {
		p, perr := u.peer(ctx, chatID)
		if perr != nil {
			return nil, perr
		}
		res, err = u.api.MessagesSearch(ctx, &tg.MessagesSearchRequest{Peer: p, Q: query, Filter: &tg.InputMessagesFilterEmpty{}, Limit: limit})
	}
	if err != nil {
		return nil, err
	}
	return u.messagesOf(res), nil
}

func randomInt64() int64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return int64(binary.LittleEndian.Uint64(b[:]) >> 1)
}

func (u *gotdUser) Send(ctx context.Context, chatID int64, text string, replyTo int) (message, error) {
	p, err := u.peer(ctx, chatID)
	if err != nil {
		return message{}, err
	}
	req := &tg.MessagesSendMessageRequest{Peer: p, Message: text, RandomID: randomInt64()}
	if replyTo != 0 {
		req.ReplyTo = &tg.InputReplyToMessage{ReplyToMsgID: replyTo}
	}
	res, err := u.api.MessagesSendMessage(ctx, req)
	if err != nil {
		return message{}, err
	}
	out := message{ChatID: chatID, Out: true, Text: text, Date: time.Now().UTC(), ReplyTo: replyTo}
	switch r := res.(type) {
	case *tg.UpdateShortSentMessage:
		out.ID = r.ID
	case *tg.Updates:
		for _, up := range r.Updates {
			if x, ok := up.(*tg.UpdateMessageID); ok && x.RandomID == req.RandomID {
				out.ID = x.ID
			}
		}
	}
	_ = u.gaps.Handle(ctx, res) // keep the update state in step with our own message
	return out, nil
}

func (u *gotdUser) SaveDraft(ctx context.Context, chatID int64, text string, replyTo int) error {
	p, err := u.peer(ctx, chatID)
	if err != nil {
		return err
	}
	req := &tg.MessagesSaveDraftRequest{Peer: p, Message: text}
	if replyTo != 0 {
		req.ReplyTo = &tg.InputReplyToMessage{ReplyToMsgID: replyTo}
	}
	_, err = u.api.MessagesSaveDraft(ctx, req)
	return err
}

func (u *gotdUser) Resolve(ctx context.Context, ref string) (chatInfo, error) {
	ref = strings.TrimSpace(ref)
	if strings.EqualFold(ref, "me") || strings.EqualFold(ref, "saved messages") {
		ref = strconv.FormatInt(u.selfID, 10)
	}
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		if _, err := u.peer(ctx, id); err != nil {
			return chatInfo{}, err
		}
		u.mu.Lock()
		defer u.mu.Unlock()
		return u.chats[id], nil
	}
	if name, ok := strings.CutPrefix(ref, "@"); ok {
		res, err := u.api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: name})
		if err != nil {
			if tgerr.Is(err, "USERNAME_NOT_OCCUPIED", "USERNAME_INVALID") {
				return chatInfo{}, fmt.Errorf("no Telegram user or chat @%s", name)
			}
			return chatInfo{}, err
		}
		u.learnUsers(res.Users)
		u.learnChats(res.Chats)
		id := markPeer(res.Peer)
		u.mu.Lock()
		defer u.mu.Unlock()
		return u.chats[id], nil
	}
	list, err := u.Dialogs(ctx, 100)
	if err != nil {
		return chatInfo{}, err
	}
	var found []chatInfo
	for _, c := range list {
		if strings.EqualFold(c.Title, ref) {
			found = append(found, c)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return chatInfo{}, fmt.Errorf("no recent chat titled %q; use an id from %s or an @username", ref, tool("chats"))
	}
	return chatInfo{}, fmt.Errorf("several chats are titled %q; use the id from %s", ref, tool("chats"))
}
