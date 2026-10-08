package main

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Background work. Each personal account keeps one long-lived MTProto connection while Rubi runs it,
// which receives updates (new messages) and serves the tools. Each bot polls getUpdates (long polling)
// and keeps a bounded store of recent messages, since a bot can't read history. Only the user's own Rubi
// talks to Telegram.

type integration struct {
	mu     sync.Mutex // the plugin state (load, change, save)
	logins logins

	cmu    sync.Mutex
	users  map[string]*userSlot     // account id → connection
	bots   map[string]botAPI        // account id → Bot API client
	savers map[string]*sessionSaver // account id → renewed-session writer

	cfgMu sync.Mutex
	cfg   map[string]cachedConfig // account id → user-only settings

	stop chan struct{}
	ctx  context.Context
	quit context.CancelFunc
	wg   sync.WaitGroup
}

type userSlot struct {
	mu sync.Mutex
	c  userClient
}

// pollTimeout is the getUpdates long-polling timeout in seconds.
var pollTimeout = 25

// Retry delays (tests shorten them).
var (
	retryMin = 30 * time.Second
	retryMax = 30 * time.Minute
)

func (x *integration) Start(b base) error {
	x.cmu.Lock()
	if x.stop != nil {
		x.cmu.Unlock()
		return nil
	}
	x.stop = make(chan struct{})
	x.ctx, x.quit = context.WithCancel(context.Background())
	stop, ctx := x.stop, x.ctx
	x.cmu.Unlock()
	list, err := accounts(x.wrap(b))
	if err != nil {
		return err
	}
	for _, a := range list {
		x.wg.Add(1)
		if a.isBot() {
			go func() { defer x.wg.Done(); x.botLoop(ctx, a, stop) }()
		} else {
			go func() { defer x.wg.Done(); x.userLoop(ctx, a, stop) }()
		}
	}
	return nil
}

func (x *integration) Stop() {
	x.cmu.Lock()
	if x.stop != nil {
		close(x.stop)
		x.quit()
		x.stop = nil
	}
	slots, savers := x.users, x.savers
	x.users, x.bots, x.savers = nil, nil, nil
	x.cmu.Unlock()
	for _, s := range slots {
		s.mu.Lock()
		if s.c != nil {
			s.c.Close()
			s.c = nil
		}
		s.mu.Unlock()
	}
	x.wg.Wait()
	for _, s := range savers {
		s.flush() // don't lose a session renewed just before stopping
	}
	x.logins.closeAll()
	x.configChanged("")
}

func sleep(stop <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-stop:
		return false
	case <-t.C:
		return true
	}
}

// ---- personal accounts ----

func closed(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// user returns the account's live connection, connecting if needed.
func (x *integration) user(ctx context.Context, a *account) (userClient, error) {
	x.cmu.Lock()
	if x.users == nil {
		x.users = map[string]*userSlot{}
	}
	slot := x.users[a.id]
	if slot == nil {
		slot = &userSlot{}
		x.users[a.id] = slot
	}
	x.cmu.Unlock()

	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.c != nil && !closed(slot.c.Done()) {
		return slot.c, nil
	}
	s, err := a.settings()
	if err != nil {
		return nil, err
	}
	if s.Kind != kindUserAccount {
		return nil, errors.New("this is a bot account")
	}
	hash, err := a.Secret("api_hash")
	if err != nil {
		return nil, err
	}
	sess, err := a.Secret("session")
	if err != nil {
		return nil, err
	}
	if sess == "" || hash == "" {
		return nil, errSessionRevoked
	}
	saver := x.saverFor(a, sess)
	dctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	c, err := connectUser(dctx, userConfig{APIID: s.APIID, APIHash: hash, Session: sess, OnSession: saver.store}, func(m message) {
		x.onUserMessage(a, m)
	})
	if err != nil {
		return nil, err
	}
	slot.c = c
	return c, nil
}

// onUserMessage handles a new message a personal account received.
func (x *integration) onUserMessage(a *account, m message) {
	x.mu.Lock()
	defer x.mu.Unlock()
	st, err := x.loadState(a)
	if err != nil {
		a.Logf("loading state failed: %v", err)
		return
	}
	if len(st.activeWatches(time.Now())) == 0 && len(st.Tracked) == 0 {
		return // nothing to follow
	}
	x.incoming(a, st, []message{m}, false)
	if err := a.SaveState(st); err != nil {
		a.Logf("saving state failed: %v", err)
	}
}

// userLoop keeps a personal account connected for updates, reconnecting with growing pauses.
func (x *integration) userLoop(ctx context.Context, a *account, stop <-chan struct{}) {
	delay := retryMin
	for {
		c, err := x.user(ctx, a)
		if err != nil {
			if errors.Is(err, errSessionRevoked) {
				a.Logf("%v", err)
				return
			}
			a.Logf("connecting failed (retrying in %s): %v", delay, err)
			if !sleep(stop, delay) {
				return
			}
			delay = min(delay*2, retryMax)
			continue
		}
		delay = retryMin
		select {
		case <-stop:
			return
		case <-c.Done():
			if errors.Is(c.Err(), errSessionRevoked) {
				a.Logf("%v", c.Err())
				return
			}
			a.Logf("connection ended: %v", c.Err())
			if !sleep(stop, retryMin) {
				return
			}
		}
	}
}

// ---- bots ----

func (x *integration) botAPIFor(a *account) (botAPI, error) {
	x.cmu.Lock()
	defer x.cmu.Unlock()
	if api := x.bots[a.id]; api != nil {
		return api, nil
	}
	token, err := a.Secret("bot_token")
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, errors.New("this bot has no token stored; connect it again in the Rubi panel")
	}
	api := newBotAPI(token)
	if x.bots == nil {
		x.bots = map[string]botAPI{}
	}
	x.bots[a.id] = api
	return api, nil
}

func (x *integration) botLoop(ctx context.Context, a *account, stop <-chan struct{}) {
	s, err := a.settings()
	if err != nil {
		a.Logf("settings unavailable: %v", err)
		return
	}
	delay := 5 * time.Second
	for {
		select {
		case <-stop:
			return
		default:
		}
		api, err := x.botAPIFor(a)
		if err == nil {
			err = x.pollBot(ctx, a, api, s.ID, pollTimeout)
		}
		if err == nil {
			delay = 5 * time.Second
			continue
		}
		if ctx.Err() != nil {
			return
		}
		wait := delay
		var be *botError
		if errors.As(err, &be) && be.RetryAfter > 0 {
			wait = time.Duration(be.RetryAfter) * time.Second
		}
		a.Logf("getUpdates failed (retrying in %s): %v", wait, err)
		if !sleep(stop, wait) {
			return
		}
		delay = min(delay*2, 5*time.Minute)
	}
}

// pollBot fetches the bot's new updates once, stores their messages and handles them.
func (x *integration) pollBot(ctx context.Context, a *account, api botAPI, botID int64, timeout int) error {
	x.mu.Lock()
	st, err := x.loadState(a)
	x.mu.Unlock()
	if err != nil {
		return err
	}
	var offset int64
	if st.Bot != nil {
		offset = st.Bot.Offset
	}
	ups, err := api.GetUpdates(ctx, offset, timeout)
	if err != nil || len(ups) == 0 {
		return err
	}

	x.mu.Lock()
	defer x.mu.Unlock()
	st, err = x.loadState(a) // reload: a tool may have changed it meanwhile
	if err != nil {
		return err
	}
	if st.Bot == nil {
		st.Bot = &botStore{}
	}
	var fresh []message
	for _, u := range ups {
		st.Bot.Offset = max(st.Bot.Offset, u.UpdateID+1)
		switch {
		case u.Message != nil:
			fresh = append(fresh, u.Message.toMessage(botID))
			st.Bot.add(fresh[len(fresh)-1])
		case u.ChannelPost != nil:
			fresh = append(fresh, u.ChannelPost.toMessage(botID))
			st.Bot.add(fresh[len(fresh)-1])
		case u.EditedMessage != nil:
			st.Bot.add(u.EditedMessage.toMessage(botID)) // stored, but doesn't wake anyone again
		}
	}
	st.prune(time.Now())
	x.incoming(a, st, fresh, true)
	return a.SaveState(st)
}
