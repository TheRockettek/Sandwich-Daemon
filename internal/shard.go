package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/TheRockettek/Sandwich-Daemon/pkg/snowflake"
	"github.com/TheRockettek/Sandwich-Daemon/structs"
	discord "github.com/TheRockettek/Sandwich-Daemon/structs/discord"
	"github.com/TheRockettek/czlib"
	"github.com/andybalholm/brotli"
	"github.com/rs/zerolog"
	"github.com/savsgio/gotils"
	"github.com/tevino/abool"
	"golang.org/x/xerrors"
	"nhooyr.io/websocket"
)

const (
	timeoutDuration     = 2 * time.Second
	dispatchTimeout     = 30 * time.Second
	waitForReadyTimeout = 10 * time.Second
	identifyRatelimit   = (5 * time.Second) + (500 * time.Millisecond)

	websocketReadLimit    = 512 << 20
	reconnectCloseCode    = 4000
	maxReconnectWait      = 600
	gatewayConnectTimeout = 5

	messageChannelBuffer      = 64
	minPayloadCompressionSize = 1000000 // Apply higher level compression to payloads >1 Mb

	// Time necessary to abort chunking if no event received in this timeframe.
	initialMemberChunkTimeout = 10 * time.Second

	// Time necessary to mark chunking as completed if no more events received in this timeframe.
	memberChunkTimeout = 1 * time.Second

	// Time between chunks before marked as no longer chunked.
	chunkStatePersistTimeout = 10 * time.Second
)

// Shard represents the shard object.
type Shard struct {
	sync.RWMutex // used to lock less common variables such as the user

	Status   structs.ShardStatus `json:"status"`
	StatusMu sync.RWMutex        `json:"-"`

	Logger zerolog.Logger `json:"-"`

	ShardID    int         `json:"shard_id"`
	ShardGroup *ShardGroup `json:"-"`
	Manager    *Manager    `json:"-"`

	User *discord.User `json:"user"`
	// Todo: Add deque that can allow for an event queue (maybe).

	ctx    context.Context
	cancel func()

	HeartbeatActive   *abool.AtomicBool `json:"-"`
	LastHeartbeatMu   sync.RWMutex      `json:"-"`
	LastHeartbeatAck  time.Time         `json:"last_heartbeat_ack"`
	LastHeartbeatSent time.Time         `json:"last_heartbeat_sent"`

	Heartbeater          *time.Ticker  `json:"-"`
	HeartbeatInterval    time.Duration `json:"heartbeat_interval"`
	MaxHeartbeatFailures time.Duration `json:"max_heartbeat_failures"`

	UnavailableMu sync.RWMutex          `json:"-"`
	Unavailable   map[snowflake.ID]bool `json:"-"`

	Start   time.Time `json:"start"`
	Retries *int32    `json:"retries"` // When erroring, how many times to retry connecting until shardgroup is stopped.

	FastCompressor    sync.Pool
	DefaultCompressor sync.Pool

	wsConn *websocket.Conn

	mp sync.Pool
	rp sync.Pool
	pp sync.Pool
	cp sync.Pool

	MessageCh chan discord.ReceivedPayload
	ErrorCh   chan error

	events *int64

	seq       *int64
	sessionID string

	// Channel that dictates if the shard has been made ready.
	ready chan void

	// Channel to pipe errors.
	errs chan error
}

// NewShard creates a new shard object.
func (sg *ShardGroup) NewShard(shardID int) *Shard {
	logger := sg.Logger.With().Int("shard", shardID).Logger()
	sh := &Shard{
		Status:   structs.ShardIdle,
		StatusMu: sync.RWMutex{},

		Logger: logger,

		ShardID:    shardID,
		ShardGroup: sg,
		Manager:    sg.Manager,

		HeartbeatActive:   abool.New(),
		LastHeartbeatMu:   sync.RWMutex{},
		LastHeartbeatAck:  time.Now().UTC(),
		LastHeartbeatSent: time.Now().UTC(),

		UnavailableMu: sync.RWMutex{},

		Start:   time.Now().UTC(),
		Retries: new(int32),

		FastCompressor: sync.Pool{
			New: func() interface{} { return brotli.NewWriterLevel(nil, brotli.BestSpeed) },
		},

		DefaultCompressor: sync.Pool{
			New: func() interface{} { return brotli.NewWriterLevel(nil, brotli.DefaultCompression) },
		},

		// Pool of payloads from discord
		mp: sync.Pool{
			New: func() interface{} { return new(discord.ReceivedPayload) },
		},

		// Pool of payloads sent to discord
		rp: sync.Pool{
			New: func() interface{} { return new(discord.SentPayload) },
		},

		// Pool of payloads sent to consumers
		pp: sync.Pool{
			New: func() interface{} { return new(structs.SandwichPayload) },
		},

		// Pool for storing Buffers
		cp: sync.Pool{
			New: func() interface{} { return new(bytes.Buffer) },
		},

		events: new(int64),

		seq:       new(int64),
		sessionID: "",

		ready: make(chan void, 1),

		errs: make(chan error),
	}

	if sh.ctx == nil || sh.cancel == nil {
		sh.ctx, sh.cancel = context.WithCancel(context.Background())
	}

	atomic.StoreInt32(sh.Retries, sg.Manager.Configuration.Bot.Retries)

	return sh
}

// Open starts up the shard connection.
func (sh *Shard) Open() {
	sh.Logger.Debug().Msg("Opening Shard")

	for {
		sh.Logger.Debug().Msg("Started listening to shard")

		err := sh.Listen()
		if xerrors.Is(err, context.Canceled) {
			sh.Logger.Debug().Msg("Shard received context Canceled")

			return
		}

		// Check if context is done
		select {
		case <-sh.ctx.Done():
			sh.Logger.Debug().Msg("Shard context has finished")

			return
		default:
		}
	}
}

// Connect connects to the gateway such as identifying however does not listen to new messages.
func (sh *Shard) Connect() (err error) {
	sh.Logger.Debug().Msg("Starting shard")

	if err := sh.SetStatus(structs.ShardWaiting); err != nil {
		sh.Logger.Error().Err(err).Msg("Encountered error setting shard status")
	}

	sh.Manager.GatewayMu.RLock()

	// Fetch the current bucket we should be using for concurrency.
	concurrencyBucket := sh.ShardID % sh.Manager.Gateway.SessionStartLimit.MaxConcurrency

	sh.Logger.Trace().Msgf("Using concurrency bucket %d", concurrencyBucket)

	// if _, ok := sh.ShardGroup.IdentifyBucket[concurrencyBucket]; !ok {
	// 	sh.Logger.Trace().Msgf("Creating new concurrency bucket %d", concurrencyBucket)
	// 	sh.ShardGroup.IdentifyBucket[concurrencyBucket] = &sync.Mutex{}
	// }

	sh.Manager.GatewayMu.RUnlock()

	// If the context has canceled, create new context.
	select {
	case <-sh.ctx.Done():
		sh.Logger.Trace().Msg("Creating new context")
		sh.ctx, sh.cancel = context.WithCancel(context.Background())
	default:
		sh.Logger.Trace().Msg("No need for new context")
	}

	// Create and wait for the websocket bucket.
	sh.Logger.Trace().Msg("Creating buckets")
	sh.Manager.Buckets.CreateBucket(fmt.Sprintf("ws:%d:%d", sh.ShardID, sh.ShardGroup.ShardCount), 120, time.Minute)

	hash, err := QuickHash(sh.Manager.Configuration.Token)
	if err != nil {
		sh.Logger.Error().Err(err).Msg("Failed to generate token hash")

		return err
	}

	sh.Manager.Sandwich.Buckets.CreateBucket(fmt.Sprintf("gw:%s:%d", hash, concurrencyBucket), 1, identifyRatelimit)

	// When an error occurs and we have to reconnect, we make a ready channel by default
	// which seems to cause a problem with WaitForReady. To circumvent this, we will
	// make the ready only when the channel is closed however this may not be necessary
	// as there is now a loop that fires every 10 seconds meaning it will be up to date regardless.

	if sh.ready == nil {
		sh.ready = make(chan void, 1)
	}

	sh.Manager.GatewayMu.RLock()
	gatewayURL := sh.Manager.Gateway.URL
	sh.Manager.GatewayMu.RUnlock()

	defer func() {
		if err != nil && sh.wsConn != nil {
			if _err := sh.CloseWS(websocket.StatusNormalClosure); _err != nil {
				sh.Logger.Error().Err(_err).Msg("Failed to close websocket")
			}
		}
	}()

	sh.Logger.Trace().Msg("Starting connecting")

	if err := sh.SetStatus(structs.ShardConnecting); err != nil {
		sh.Logger.Error().Err(err).Msg("Encountered error setting shard status")
	}

	// If there is no active ws connection, create a new connection to discord.
	if sh.wsConn == nil {
		var errorCh chan error

		var messageCh chan discord.ReceivedPayload

		errorCh, messageCh, err = sh.FeedWebsocket(sh.ctx, gatewayURL, nil)
		if err != nil {
			sh.Logger.Error().Err(err).Msg("Failed to dial")

			go sh.PublishWebhook(fmt.Sprintf("Failed to dial `%s`", gatewayURL), err.Error(), 14431557, false)

			return
		}

		sh.Lock()
		sh.ErrorCh = errorCh
		sh.MessageCh = messageCh
		sh.Unlock()
	} else {
		sh.Logger.Info().Msg("Reusing websocket connection")
	}

	sh.Logger.Trace().Msg("Reading from WS")

	// Read a message from WS which we should expect to be Hello
	msg, err := sh.readMessage()
	if err != nil {
		sh.Logger.Error().Err(err).Msg("Failed to read message")

		return
	}

	hello := discord.Hello{}
	err = sh.decodeContent(msg, &hello)

	sh.LastHeartbeatMu.Lock()
	sh.LastHeartbeatAck = time.Now().UTC()
	sh.LastHeartbeatSent = time.Now().UTC()
	sh.LastHeartbeatMu.Unlock()

	sh.Lock()
	sh.HeartbeatInterval = hello.HeartbeatInterval * time.Millisecond
	sh.MaxHeartbeatFailures = sh.HeartbeatInterval * time.Duration(sh.Manager.Configuration.Bot.MaxHeartbeatFailures)
	sh.Heartbeater = time.NewTicker(sh.HeartbeatInterval)
	sh.Unlock()

	if sh.HeartbeatActive.IsNotSet() {
		go sh.Heartbeat()
	}

	seq := atomic.LoadInt64(sh.seq)

	sh.Logger.Debug().
		Dur("interval", sh.HeartbeatInterval).
		Int("maxfails", sh.Manager.Configuration.Bot.MaxHeartbeatFailures).
		Msg("Retrieved HELLO event from discord")

	// If we have no session ID or the sequence is 0, we can identify instead
	// of resuming.
	sh.RLock()
	sessionID := sh.sessionID
	sh.RUnlock()

	if sessionID == "" || seq == 0 {
		err = sh.Identify()
		if err != nil {
			sh.Logger.Error().Err(err).Msg("Failed to identify")

			go sh.PublishWebhook("Gateway `IDENTIFY` failed", err.Error(), 14431557, false)

			return
		}
	} else {
		err = sh.Resume()
		if err != nil {
			sh.Logger.Error().Err(err).Msg("Failed to resume")

			go sh.PublishWebhook("Gateway `RESUME` failed", err.Error(), 14431557, false)

			return
		}

		// We will assume the bot is ready.
		if err := sh.SetStatus(structs.ShardReady); err != nil {
			sh.Logger.Error().Err(err).Msg("Encountered error setting shard status")
		}
	}

	sh.Manager.ConfigurationMu.RLock()
	hash, err = QuickHash(sh.Manager.Configuration.Token)

	if err != nil {
		sh.Manager.ConfigurationMu.RUnlock()
		sh.Logger.Error().Err(err).Msg("Failed to generate token hash")

		return
	}

	// Reset the bucket we used for gateway
	bucket := fmt.Sprintf("gw:%s:%d", hash, sh.ShardID%sh.Manager.Gateway.SessionStartLimit.MaxConcurrency)
	sh.Manager.Buckets.ResetBucket(bucket)
	sh.Manager.ConfigurationMu.RUnlock()

	t := time.NewTicker(time.Second * gatewayConnectTimeout)

	// Wait 5 seconds for the first event or errors in websocket to
	// ensure there are no error messages such as disallowed intents.

	sh.Logger.Trace().Msg("Waiting for first event")

	sh.RLock()
	errorch := sh.ErrorCh
	messagech := sh.MessageCh
	sh.RUnlock()

	select {
	case err = <-errorch:
		sh.Logger.Error().Err(err).Msg("Encountered error whilst connecting")

		go sh.PublishWebhook("Encountered error during connection", err.Error(), 14431557, false)

		return xerrors.Errorf("encountered error whilst connecting: %w", err)
	case msg = <-messagech:
		sh.Logger.Debug().Msgf("Received first event. %d %s", msg.Op, msg.Type)

		// Requeue event so main loop can handle it
		messagech <- msg
	case <-t.C:
	}

	sh.Logger.Trace().Msg("Finished connecting")

	return err
}

// FeedWebsocket reads websocket events and feeds them through a channel.
func (sh *Shard) FeedWebsocket(ctx context.Context, u string,
	opts *websocket.DialOptions) (errorCh chan error, messageCh chan discord.ReceivedPayload, err error) {
	messageCh = make(chan discord.ReceivedPayload, messageChannelBuffer)
	errorCh = make(chan error, 1)

	conn, _, err := websocket.Dial(ctx, u, opts)
	if err != nil {
		sh.Logger.Error().Err(err).Msg("Failed to dial websocket")

		return errorCh, messageCh, xerrors.Errorf("failed to connect to websocket: %w", err)
	}

	conn.SetReadLimit(websocketReadLimit)
	sh.wsConn = conn

	go func() {
		for {
			mt, buf, err := conn.Read(ctx)

			select {
			case <-ctx.Done():
				return
			default:
			}

			if err != nil {
				errorCh <- xerrors.Errorf("readMessage read: %w", err)

				return
			}

			if mt == websocket.MessageBinary {
				buf, err = czlib.Decompress(buf)
				if err != nil {
					errorCh <- xerrors.Errorf("readMessage decompress: %w", err)

					return
				}
			}

			now := time.Now().UTC()
			msg := discord.ReceivedPayload{
				TraceTime: now,
				Trace:     make(map[string]int),
			}

			err = json.Unmarshal(buf, &msg)
			if err != nil {
				sh.Logger.Error().Err(err).Msg("Failed to unmarshal message")

				continue
			}

			now = time.Now().UTC()
			msg.AddTrace("unmarshal", now)

			atomic.AddInt64(sh.events, 1)

			messageCh <- msg
		}
	}()

	return errorCh, messageCh, nil
}

// OnEvent processes an event.
func (sh *Shard) OnEvent(msg discord.ReceivedPayload) {
	var err error

	// This goroutine shows events that are taking too long.
	fin := make(chan void)

	go func() {
		since := time.Now()
		t := time.NewTimer(dispatchTimeout)

		for {
			select {
			case <-fin:
				return
			case <-t.C:
				sh.Logger.Warn().
					Str("type", msg.Type).
					Int("op", int(msg.Op)).
					Str("data", gotils.B2S(msg.Data)).
					Msgf("Event %s is taking too long. Been executing for %f seconds. Possible deadlock?",
						msg.Type, time.Since(since).
							Round(time.Second).Seconds(),
					)
				t.Reset(dispatchTimeout)
			}
		}
	}()

	defer close(fin)

	switch msg.Op {
	case discord.GatewayOpHeartbeat:
		sh.Logger.Debug().Msg("Received heartbeat request")
		err = sh.SendEvent(discord.GatewayOpHeartbeat, atomic.LoadInt64(sh.seq))

		if err != nil {
			go sh.PublishWebhook("Failed to send heartbeat to gateway", err.Error(), 16760839, false)

			sh.Logger.Error().Err(err).Msg("Failed to send heartbeat in response to gateway, reconnecting...")
			err = sh.Reconnect(websocket.StatusNormalClosure)

			if err != nil {
				sh.Logger.Error().Err(err).Msg("Failed to reconnect")
			}

			return
		}
	case discord.GatewayOpInvalidSession:
		resumable := json.Get(msg.Data, "d").ToBool()
		if !resumable {
			sh.sessionID = ""
			atomic.StoreInt64(sh.seq, 0)
		}

		go sh.PublishWebhook("Received invalid session from gateway", "", 16760839, false)

		sh.Logger.Warn().Bool("resumable", resumable).Msg("Received invalid session from gateway")
		err = sh.Reconnect(reconnectCloseCode)

		if err != nil {
			sh.Logger.Error().Err(err).Msg("Failed to reconnect")
		}
	case discord.GatewayOpHello:
		hello := discord.Hello{}
		err = sh.decodeContent(msg, &hello)

		sh.LastHeartbeatMu.Lock()
		sh.LastHeartbeatAck = time.Now().UTC()
		sh.LastHeartbeatSent = time.Now().UTC()
		sh.LastHeartbeatMu.Unlock()

		sh.Lock()
		sh.HeartbeatInterval = hello.HeartbeatInterval * time.Millisecond
		sh.MaxHeartbeatFailures = sh.HeartbeatInterval * time.Duration(sh.Manager.Configuration.Bot.MaxHeartbeatFailures)
		sh.Heartbeater = time.NewTicker(sh.HeartbeatInterval)
		sh.Unlock()

		sh.Logger.Debug().
			Dur("interval", sh.HeartbeatInterval).
			Int("maxfails", sh.Manager.Configuration.Bot.MaxHeartbeatFailures).
			Msg("Retrieved HELLO event from discord")

		return
	case discord.GatewayOpReconnect:
		sh.Logger.Info().Msg("Reconnecting in response to gateway")
		err = sh.Reconnect(reconnectCloseCode)

		if err != nil {
			sh.Logger.Error().Err(err).Msg("Failed to reconnect")
		}

		return
	case discord.GatewayOpDispatch:
		exec := func() {
			var ticket int

			atomic.AddInt64(sh.Manager.Sandwich.PoolWaiting, 1)

			ticket = sh.Manager.Sandwich.Pool.Wait()
			defer sh.Manager.Sandwich.Pool.FreeTicket(ticket)

			msg.AddTrace("ticket", time.Now().UTC())
			msg.Trace["ticket_id"] = ticket

			atomic.AddInt64(sh.Manager.Sandwich.PoolWaiting, -1)

			err = sh.OnDispatch(msg)
			if err != nil && !xerrors.Is(err, NoHandler) {
				sh.Logger.Error().Err(err).Msg("Failed to handle event")
			}
		}

		// UNUSED:
		// To reduce the chance of race conditions, some Dispatch events block the
		// goroutine that reads from MessageCh however this does not mean we do not
		// handle messages whilst this is running! This essentially just means we pass
		// control of the MessageCh to that event for its duration. Currently this is
		// only the READY event.
		go exec()
	case discord.GatewayOpHeartbeatACK:
		sh.LastHeartbeatMu.Lock()
		sh.LastHeartbeatAck = time.Now().UTC()
		sh.Logger.Debug().
			Int64("RTT", sh.LastHeartbeatAck.Sub(sh.LastHeartbeatSent).Milliseconds()).
			Msg("Received heartbeat ACK")

		sh.LastHeartbeatMu.Unlock()

		return
	case discord.GatewayOpVoiceStateUpdate:
		// Todo: handle
	case discord.GatewayOpIdentify,
		discord.GatewayOpRequestGuildMembers,
		discord.GatewayOpResume,
		discord.GatewayOpStatusUpdate:
	default:
		sh.Logger.Warn().
			Int("op", int(msg.Op)).
			Str("type", msg.Type).
			Msg("Gateway sent unknown packet")

		return
	}

	atomic.StoreInt64(sh.seq, msg.Sequence)
}

// OnDispatch handles a dispatch event.
func (sh *Shard) OnDispatch(msg discord.ReceivedPayload) (err error) {
	start := time.Now().UTC()

	defer func() {
		now := time.Now().UTC()
		change := now.Sub(start)

		msg.AddTrace("publish", now)

		if change > time.Second {
			l := sh.Logger.Warn()

			if trcrslt, err := json.MarshalToString(msg.Trace); err == nil {
				l = l.Str("trace", trcrslt)
			}

			l.Msgf("%s took %d ms", msg.Type, change.Milliseconds())
		}

		if change > 15*time.Second {
			trcrslt := ""

			for tracer, tracetime := range msg.Trace {
				trcrslt += fmt.Sprintf("%s: **%d**ms\n", tracer, tracetime)
			}

			go sh.PublishWebhook(
				fmt.Sprintf("Packet `%s` took too long. Took `%dms`", msg.Type,
					change.Milliseconds()), trcrslt, 16760839, false)
		}
	}()

	if sh.Manager.ProducerClient == nil {
		return xerrors.Errorf("no producer client found")
	}

	// Ignore events that are in the event blacklist.
	sh.Manager.EventBlacklistMu.RLock()
	contains := gotils.StringSliceInclude(sh.Manager.EventBlacklist, msg.Type)
	sh.Manager.EventBlacklistMu.RUnlock()

	if contains {
		return
	}

	msg.AddTrace("dispatch", time.Now().UTC())

	results, ok, err := sh.Manager.Sandwich.StateDispatch(&StateCtx{
		Sg: sh.Manager.Sandwich,
		Mg: sh.Manager,
		Sh: sh,
	}, msg)

	msg.AddTrace("state", time.Now().UTC())

	if err != nil {
		return xerrors.Errorf("on dispatch failure for %s: %w", msg.Type, err)
	}

	if !ok {
		return
	}

	// Do not publish the event if it is in the produce blacklist,
	// regardless if it has been marked ok.
	sh.Manager.ProduceBlacklistMu.RLock()
	contains = gotils.StringSliceInclude(sh.Manager.ProduceBlacklist, msg.Type)
	sh.Manager.ProduceBlacklistMu.RUnlock()

	if contains {
		return
	}

	packet := sh.pp.Get().(*structs.SandwichPayload)
	defer sh.pp.Put(packet)

	packet.ReceivedPayload = msg
	packet.Trace = msg.Trace
	packet.Data = results.Data
	packet.Extra = results.Extra

	err = sh.PublishEvent(packet)

	return err
}

// Listen to gateway and process accordingly.
func (sh *Shard) Listen() (err error) {
	wsConn := sh.wsConn

	for {
		select {
		case <-sh.ctx.Done():
			return
		default:
		}

		msg, err := sh.readMessage()
		if err != nil {
			if xerrors.Is(err, context.Canceled) || xerrors.Is(err, context.DeadlineExceeded) {
				break
			}

			sh.Logger.Error().Err(err).Msg("Error reading from gateway")

			var closeError *websocket.CloseError

			if errors.As(err, &closeError) {
				// If possible, we will check the close error to determine if we can continue
				switch closeError.Code {
				case discord.CloseNotAuthenticated, // Not authenticated
					discord.CloseInvalidShard,      // Invalid shard
					discord.CloseShardingRequired,  // Sharding required
					discord.CloseInvalidAPIVersion, // Invalid API version
					discord.CloseInvalidIntents,    // Invalid Intent(s)
					discord.CloseDisallowedIntents: // Disallowed intent(s)
					sh.Logger.Warn().Msgf(
						"Closing ShardGroup as cannot continue without valid token. Received code %d",
						closeError.Code,
					)

					go sh.PublishWebhook("ShardGroup is closing due to invalid token being passed", "", 16760839, false)

					// We cannot continue so we will kill the ShardGroup
					sh.ShardGroup.ErrorMu.Lock()
					sh.ShardGroup.Error = err.Error()
					sh.ShardGroup.ErrorMu.Unlock()
					sh.ShardGroup.Close()

					if err := sh.ShardGroup.SetStatus(structs.ShardGroupError); err != nil {
						sh.ShardGroup.Logger.Error().Err(err).Msg("Encountered error setting shard group status")
					}

					return err
				default:
					sh.Logger.Warn().Msgf("Websocket was closed with code %d", closeError.Code)
				}
			}

			if wsConn == sh.wsConn {
				// We have likely closed so we should attempt to reconnect
				sh.Logger.Warn().Msg("We have encountered an error whilst in the same connection, reconnecting...")
				err = sh.Reconnect(websocket.StatusNormalClosure)

				if err != nil {
					return err
				}

				return nil
			}

			wsConn = sh.wsConn
		}

		sh.OnEvent(msg)

		// In the event we have reconnected, the wsConn could have changed,
		// we will use the new wsConn if this is the case
		if sh.wsConn != wsConn {
			sh.Logger.Debug().Msg("New wsConn was assigned to shard")
			wsConn = sh.wsConn
		}
	}

	return err
}

// Heartbeat maintains a heartbeat with discord
// TODO: Make a shardgroup specific heartbeat function to heartbeat on behalf of all running shards.
func (sh *Shard) Heartbeat() {
	sh.HeartbeatActive.Set()
	defer sh.HeartbeatActive.UnSet()

	for {
		sh.RLock()
		heartbeater := sh.Heartbeater
		sh.RUnlock()

		select {
		case <-sh.ctx.Done():
			return
		case <-heartbeater.C:
			sh.Logger.Debug().Msg("Heartbeating")
			seq := atomic.LoadInt64(sh.seq)

			err := sh.SendEvent(discord.GatewayOpHeartbeat, seq)

			sh.LastHeartbeatMu.Lock()
			_time := time.Now().UTC()
			sh.LastHeartbeatSent = _time
			lastAck := sh.LastHeartbeatAck
			sh.LastHeartbeatMu.Unlock()

			if err != nil || _time.Sub(lastAck) > sh.MaxHeartbeatFailures {
				if err != nil {
					sh.Logger.Error().Err(err).Msg("Failed to heartbeat. Reconnecting")

					go sh.PublishWebhook("Failed to heartbeat. Reconnecting", "", 16760839, false)
				} else {
					sh.Manager.Sandwich.ConfigurationMu.RLock()
					sh.Logger.Warn().Err(err).
						Msgf(
							"Gateway failed to ACK and has passed MaxHeartbeatFailures of %d. Reconnecting",
							sh.Manager.Configuration.Bot.MaxHeartbeatFailures)

					go sh.PublishWebhook(fmt.Sprintf(
						"Gateway failed to ACK and has passed MaxHeartbeatFailures of %d. Reconnecting",
						sh.Manager.Configuration.Bot.MaxHeartbeatFailures), "", 1548214, false)

					sh.Manager.Sandwich.ConfigurationMu.RUnlock()
				}

				err = sh.Reconnect(websocket.StatusNormalClosure)
				if err != nil {
					sh.Logger.Error().Err(err).Msg("Failed to reconnect")
				}

				return
			}
		}
	}
}

// decodeContent converts the stored msg into the passed interface.
func (sh *Shard) decodeContent(msg discord.ReceivedPayload, out interface{}) (err error) {
	err = json.Unmarshal(msg.Data, &out)

	return
}

// readMessage fills the shard msg buffer from a websocket message.
func (sh *Shard) readMessage() (msg discord.ReceivedPayload, err error) {
	// Prioritize errors
	select {
	case err = <-sh.ErrorCh:
		return msg, err
	default:
	}

	sh.RLock()
	errorch := sh.ErrorCh
	messagech := sh.MessageCh
	sh.RUnlock()

	select {
	case err = <-errorch:
		return msg, err
	case msg = <-messagech:
		msg.AddTrace("read", time.Now().UTC())

		return msg, nil
	}
}

// CloseWS closes the websocket. This will always return 0 as the error is suppressed.
func (sh *Shard) CloseWS(statusCode websocket.StatusCode) (err error) {
	if sh.wsConn != nil {
		sh.Logger.Debug().Str("code", statusCode.String()).Msg("Closing websocket connection")

		err = sh.wsConn.Close(statusCode, "")
		if err != nil && !xerrors.Is(err, context.Canceled) {
			sh.Logger.Warn().Err(err).Msg("Failed to close websocket connection")
		}

		sh.wsConn = nil
	}

	return nil
}

// Resume sends the resume packet to gateway.
func (sh *Shard) Resume() (err error) {
	sh.Logger.Debug().Msg("Sending resume")

	sh.Manager.Sandwich.ConfigurationMu.RLock()
	defer sh.Manager.Sandwich.ConfigurationMu.RUnlock()

	sh.Manager.ConfigurationMu.RLock()
	defer sh.Manager.ConfigurationMu.RUnlock()

	err = sh.SendEvent(discord.GatewayOpResume, discord.Resume{
		Token:     sh.Manager.Configuration.Token,
		SessionID: sh.sessionID,
		Sequence:  atomic.LoadInt64(sh.seq),
	})

	return
}

// Identify sends the identify packet to gateway.
func (sh *Shard) Identify() (err error) {
	sh.Manager.GatewayMu.Lock()
	sh.Manager.Gateway.SessionStartLimit.Remaining--
	sh.Manager.GatewayMu.Unlock()

	sh.Manager.ConfigurationMu.RLock()
	defer sh.Manager.ConfigurationMu.RUnlock()

	hash, err := QuickHash(sh.Manager.Configuration.Token)
	if err != nil {
		sh.Logger.Error().Err(err).Msg("Failed to generate token hash")

		return err
	}

	sh.Manager.GatewayMu.RLock()
	err = sh.Manager.Sandwich.Buckets.WaitForBucket(
		fmt.Sprintf("gw:%s:%d", hash, sh.ShardID%sh.Manager.Gateway.SessionStartLimit.MaxConcurrency),
	)
	sh.Manager.GatewayMu.RUnlock()

	sh.Logger.Debug().Msg("Sending identify")

	if err != nil {
		sh.Logger.Error().Err(err).Msg("Failed to wait for bucket")
	}

	err = sh.SendEvent(discord.GatewayOpIdentify, discord.Identify{
		Token: sh.Manager.Configuration.Token,
		Properties: &discord.IdentifyProperties{
			OS:      runtime.GOOS,
			Browser: "Sandwich " + VERSION,
			Device:  "Sandwich " + VERSION,
		},
		Compress:           sh.Manager.Configuration.Bot.Compression,
		LargeThreshold:     sh.Manager.Configuration.Bot.LargeThreshold,
		Shard:              [2]int{sh.ShardID, sh.ShardGroup.ShardCount},
		Presence:           sh.Manager.Configuration.Bot.DefaultPresence,
		GuildSubscriptions: sh.Manager.Configuration.Bot.GuildSubscriptions,
		Intents:            sh.Manager.Configuration.Bot.Intents,
	})

	return err
}

// SendEvent sends an event to discord.
func (sh *Shard) SendEvent(op discord.GatewayOp, data interface{}) (err error) {
	packet := sh.rp.Get().(*discord.SentPayload)
	defer sh.rp.Put(packet)

	packet.Op = int(op)
	packet.Data = data

	err = sh.WriteJSON(op, packet)
	if err != nil {
		return xerrors.Errorf("sendEvent writeJson: %w", err)
	}

	return
}

// WriteJSON writes json data to the websocket.
func (sh *Shard) WriteJSON(op discord.GatewayOp, i interface{}) (err error) {
	res, err := json.Marshal(i)
	if err != nil {
		return xerrors.Errorf("writeJSON marshal: %w", err)
	}

	// We will bypass the WS bucket when it is a heartbeat.
	// We do this to always ensure that heartbeat is not blocked if we are fetching
	// member chunks, for example. To still ensure we are not passing the 120 messages
	// per minute ratelimit on the gateway, we only allow up to 115 messages a minute
	// for non heartbeat messages. We should only really make it 118 in cases where it
	// heartbeats twice in a minute but allowing up to 5 a minute is more safe.
	if i != discord.GatewayOpHeartbeat {
		err = sh.Manager.Buckets.WaitForBucket(
			fmt.Sprintf("ws:%d:%d", sh.ShardID, sh.ShardGroup.ShardCount),
		)
		if err != nil {
			sh.Logger.Warn().Err(err).Msg("Tried to wait for websocket bucket but it does not exist")
		}
	}

	sh.Manager.Sandwich.ConfigurationMu.RLock()
	sh.Logger.Trace().Msg(strings.ReplaceAll(gotils.B2S(res), sh.Manager.Configuration.Token, "..."))
	sh.Manager.Sandwich.ConfigurationMu.RUnlock()

	if sh.wsConn != nil {
		err = sh.wsConn.Write(sh.ctx, websocket.MessageText, res)
		if err != nil {
			return xerrors.Errorf("writeJSON write: %w", err)
		}
	}

	return nil
}

// WaitForReady waits until the shard is ready.
func (sh *Shard) WaitForReady() {
	since := time.Now().UTC()
	t := time.NewTicker(waitForReadyTimeout)

	for {
		select {
		case <-sh.ready:
			sh.Logger.Debug().Msg("Shard ready due to channel closure")

			return
		case <-sh.ctx.Done():
			sh.Logger.Debug().Msg("Shard ready due to context done")

			return
		case <-t.C:
			sh.StatusMu.RLock()
			status := sh.Status
			sh.StatusMu.RUnlock()

			if status == structs.ShardReady {
				sh.Logger.Warn().Msg("Shard ready due to status change")

				return
			}

			sh.Logger.Debug().
				Err(sh.ctx.Err()).
				Dur("since", time.Now().UTC().Sub(since).Round(time.Second)).
				Msg("Still waiting for shard to be ready")
		}
	}
}

// Reconnect attempts to reconnect to the gateway.
func (sh *Shard) Reconnect(code websocket.StatusCode) error {
	wait := time.Second

	sh.Close(code)

	if err := sh.SetStatus(structs.ShardReconnecting); err != nil {
		sh.Logger.Error().Err(err).Msg("Encountered error setting shard status")
	}

	for {
		sh.Logger.Info().Msg("Trying to reconnect to gateway")

		err := sh.Connect()
		if err == nil {
			atomic.StoreInt32(sh.Retries, sh.Manager.Configuration.Bot.Retries)
			sh.Logger.Info().Msg("Successfully reconnected to gateway")

			return nil
		}

		retries := atomic.AddInt32(sh.Retries, -1)
		if retries <= 0 {
			sh.Logger.Warn().Msg("Ran out of retries whilst connecting. Attempting to reconnect client.")
			sh.Close(code)

			err = sh.Connect()
			if err != nil {
				go sh.PublishWebhook("Failed to reconnect to gateway", err.Error(), 14431557, false)
			}

			return err
		}

		sh.Logger.Warn().Err(err).Dur("retry", wait).Msg("Failed to reconnect to gateway")
		<-time.After(wait)

		wait *= 2
		if wait > maxReconnectWait {
			wait = maxReconnectWait
		}
	}
}

// SetStatus changes the Shard status.
func (sh *Shard) SetStatus(status structs.ShardStatus) (err error) {
	sh.StatusMu.Lock()
	sh.Status = status
	sh.StatusMu.Unlock()

	sh.Logger.Debug().
		Str("manager", sh.Manager.Configuration.Identifier).
		Int32("shardgroup", sh.ShardGroup.ID).
		Int("shard", sh.ShardID).
		Msgf("Status changed to %s (%d)", status.String(), status)

	switch status {
	case structs.ShardReady, structs.ShardReconnecting:
		sh.Manager.ConfigurationMu.RLock()
		isMinimal := sh.Manager.Sandwich.Configuration.Logging.MinimalWebhooks
		sh.Manager.ConfigurationMu.RUnlock()

		go sh.PublishWebhook(fmt.Sprintf("Shard is now **%s**", status.String()), "", status.Colour(), isMinimal)
	case structs.ShardIdle,
		structs.ShardWaiting,
		structs.ShardConnecting,
		structs.ShardConnected,
		structs.ShardClosed:
	}

	packet := sh.pp.Get().(*structs.SandwichPayload)
	defer sh.pp.Put(packet)

	packet.ReceivedPayload = discord.ReceivedPayload{
		Type: "SHARD_STATUS",
	}

	packet.Data = structs.MessagingStatusUpdate{
		ShardID: sh.ShardID,
		Status:  int32(status),
	}

	return sh.PublishEvent(packet)
}

// Latency returns the heartbeat latency in milliseconds.
func (sh *Shard) Latency() (latency int64) {
	sh.LastHeartbeatMu.RLock()
	defer sh.LastHeartbeatMu.RUnlock()

	return sh.LastHeartbeatAck.Sub(sh.LastHeartbeatSent).Round(time.Millisecond).Milliseconds()
}

// Close closes the shard connection.
func (sh *Shard) Close(code websocket.StatusCode) {
	// Ensure that if we close during shardgroup connecting, it will not
	// feedback loop.
	// cancel is only defined when Connect() has been ran on a shard.
	// If the ShardGroup was closed before this happens, it would segmentation fault.
	if sh.ctx != nil && sh.cancel != nil {
		sh.cancel()
	}

	if sh.wsConn != nil {
		if err := sh.CloseWS(code); err != nil {
			// It is highly common we are closing an already closed websocket
			// and at this point if we error closing it, its fair game. It would
			// be nice if the errAlreadyWroteClose error was public in the websocket
			// library so we could only suppress that error but what can you do.
			sh.Logger.Debug().Err(err).Msg("Encountered error closing websocket")
		}
	}

	if err := sh.SetStatus(structs.ShardClosed); err != nil {
		sh.Logger.Error().Err(err).Msg("Encountered error setting shard status")
	}
}

// ChunkGuild requests guild chunks for a guild.
func (sh *Shard) ChunkGuild(guildID snowflake.ID, wait bool) (err error) {
	sh.ShardGroup.MemberChunksCompleteMu.RLock()
	completed, ok := sh.ShardGroup.MemberChunksComplete[guildID]
	sh.ShardGroup.MemberChunksCompleteMu.RUnlock()

	// If we find a MemberChunksComplete
	//     If it is set
	//         Noop and continue
	//     If it is not set get the ChunksCallback
	//         If ChunksCallback exists then .Wait on it
	//         Else warn as a Complete should exist with Callback
	//     If The ChunksComplete does not exist
	//         Chunk the guild

	if ok {
		if !completed.IsSet() {
			sh.ShardGroup.MemberChunksCallbackMu.RLock()
			chunksCallback, ok := sh.ShardGroup.MemberChunksCallback[guildID]
			sh.ShardGroup.MemberChunksCallbackMu.RUnlock()

			if ok {
				sh.Logger.Debug().
					Int("guild_id", int(guildID.Int64())).
					Msg("Received ChunksCallback WaitGroup. Waiting...")
				chunksCallback.Wait()
			} else {
				sh.Logger.Warn().
					Int64("guild_id", guildID.Int64()).
					Msg("ChunksComplete found however no ChunksCallback existed.")
			}
		}
	} else {
		if wait {
			return sh.chunkGuild(guildID, false)
		}

		go sh.chunkGuild(guildID, true) // nolint:errcheck
	}

	return nil
}

// cleanGuildChunks all traces of a guild from the member chunking
// state maps.
func (sh *Shard) cleanGuildChunks(guildID snowflake.ID) {
	sh.ShardGroup.MemberChunksCallbackMu.Lock()
	delete(sh.ShardGroup.MemberChunksCallback, guildID)
	sh.ShardGroup.MemberChunksCallbackMu.Unlock()

	sh.ShardGroup.MemberChunkCallbacksMu.Lock()
	close(sh.ShardGroup.MemberChunkCallbacks[guildID])
	delete(sh.ShardGroup.MemberChunkCallbacks, guildID)
	sh.ShardGroup.MemberChunkCallbacksMu.Unlock()

	sh.ShardGroup.MemberChunksCompleteMu.Lock()
	delete(sh.ShardGroup.MemberChunksComplete, guildID)
	sh.ShardGroup.MemberChunksCompleteMu.Unlock()
}

// chunkGuild handles managing all state and cleaning it up.
func (sh *Shard) chunkGuild(guildID snowflake.ID, waitForTicket bool) (err error) {
	var ticket int

	if waitForTicket {
		ticket = sh.ShardGroup.ChunkLimiter.Wait()

		defer func() {
			sh.ShardGroup.ChunkLimiter.FreeTicket(ticket)
		}()
	}

	start := time.Now().UTC()

	sh.Logger.Debug().
		Int("guild_id", int(guildID.Int64())).
		Msg("Preparing to chunk guild")

	// Abool so multiple processes can know if a chunk is in progress.
	// Empty: No chunk recently, chunk
	// False: Chunk is in progress
	// True:  Chunk has recently finished, no need to wait.
	completed := abool.New()

	sh.ShardGroup.MemberChunksCompleteMu.Lock()
	sh.ShardGroup.MemberChunksComplete[guildID] = completed
	sh.ShardGroup.MemberChunksCompleteMu.Unlock()

	// Channel to signify when MEMBER_CHUNKs are received by the
	// gateway as this task does not handle reading and is "stateless".
	// We inform the channel when we receive it.
	chunkCallbacks := make(chan bool)

	sh.ShardGroup.MemberChunkCallbacksMu.Lock()
	sh.ShardGroup.MemberChunkCallbacks[guildID] = chunkCallbacks
	sh.ShardGroup.MemberChunkCallbacksMu.Unlock()

	// Channel to signify when chunking has completed.
	// If we find a waitgroup, we should wait for it to be done
	// as another task is currently in control of it. Else if we
	// are the task that made it, we need to finish it then free
	// along with Complete.
	wg := &sync.WaitGroup{}
	wg.Add(1)

	sh.ShardGroup.MemberChunksCallbackMu.Lock()
	sh.ShardGroup.MemberChunksCallback[guildID] = wg
	sh.ShardGroup.MemberChunksCallbackMu.Unlock()

	err = sh.SendEvent(discord.GatewayOpRequestGuildMembers, discord.RequestGuildMembers{
		GuildID: guildID,
		Query:   "",
		Limit:   0,
	})
	if err != nil {
		sh.Logger.Error().Err(err).
			Int64("guild_id", guildID.Int64()).
			Msg("Failed to chunk guild")

		sh.cleanGuildChunks(guildID)

		return err
	}

	t := time.NewTicker(initialMemberChunkTimeout)

	select {
	case <-chunkCallbacks:
		break
	case <-t.C:
		sh.Logger.Warn().
			Int64("guild_id", guildID.Int64()).
			Msg("Timed out on initial member chunks")

		sh.cleanGuildChunks(guildID)

		return ErrChunkTimeout
	}

	t.Reset(memberChunkTimeout)

	receivedMemberChunks := 1

memberChunks:
	for {
		select {
		case <-chunkCallbacks:
			receivedMemberChunks++
			t.Reset(memberChunkTimeout)
			sh.Logger.Debug().
				Int64("guild_id", guildID.Int64()).
				Msg("Received member chunk")
		case <-t.C:
			sh.Logger.Debug().
				Int64("guild_id", guildID.Int64()).
				Int("received", receivedMemberChunks).
				Int64("duration", time.Now().UTC().Sub(start).Round(time.Millisecond).Milliseconds()).
				Msg("Timed out on member chunks")

			break memberChunks
		}
	}

	// Finish marking chunking as done and handle closing.
	wg.Done()
	completed.Set()

	go func() {
		time.Sleep(chunkStatePersistTimeout)

		sh.cleanGuildChunks(guildID)

		sh.Logger.Trace().
			Int("guild_id", int(guildID.Int64())).
			Msg("Cleaned MemberChunk tables")
	}()

	return nil
}

// PublishWebhook is the same as sg.PublishWebhook but has extra sugar for
// displaying information about the shard.
func (sh *Shard) PublishWebhook(title string, description string, colour int, raw bool) {
	var message discord.WebhookMessage

	if raw {
		message = discord.WebhookMessage{
			Content: fmt.Sprintf("[**%s - %d/%d**] %s %s",
				sh.Manager.Configuration.DisplayName,
				sh.ShardGroup.ID, sh.ShardID, title, description),
		}

		sh.RLock()
		if sh.User != nil && message.AvatarURL == "" && message.Username == "" {
			message.AvatarURL = fmt.Sprintf("https://cdn.discordapp.com/avatars/%s/%s.png",
				sh.User.ID.String(), sh.User.Avatar)
			message.Username = sh.User.Username
		}
		sh.RUnlock()
	} else {
		message = discord.WebhookMessage{
			Embeds: []discord.Embed{
				{
					Title:       title,
					Description: description,
					Color:       colour,
					Timestamp:   WebhookTime(time.Now().UTC()),
					Footer: &discord.EmbedFooter{
						Text: fmt.Sprintf("Manager %s | ShardGroup %d | Shard %d",
							sh.Manager.Configuration.DisplayName,
							sh.ShardGroup.ID, sh.ShardID),
					},
				},
			},
		}

		sh.RLock()
		if sh.User != nil && message.AvatarURL == "" && message.Username == "" {
			message.AvatarURL = fmt.Sprintf("https://cdn.discordapp.com/avatars/%s/%s.png",
				sh.User.ID.String(), sh.User.Avatar)
			message.Username = sh.User.Username
		}
		sh.RUnlock()
	}

	sh.Manager.Sandwich.PublishWebhook(context.Background(), message)
}
