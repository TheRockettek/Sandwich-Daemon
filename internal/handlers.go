package gateway

import (
	"compress/flate"
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/TheRockettek/Sandwich-Daemon/internal/mqclients"
	methodrouter "github.com/TheRockettek/Sandwich-Daemon/pkg/methodrouter"
	"github.com/TheRockettek/Sandwich-Daemon/structs"
	"github.com/fasthttp/websocket"
	"github.com/gorilla/sessions"
	"github.com/hashicorp/go-uuid"
	"github.com/rs/zerolog"
	"github.com/savsgio/gotils"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpadaptor"
)

const (
	// apiSubscribeDuration is the time in seconds between each API Subscribe WS message.
	apiSubscribeDuration = 15

	sessionName      = "session"
	forbiddenMessage = "You are not elevated"

	discordUsersMe = "https://discord.com/api/users/@me"
)

var upgrader = websocket.FastHTTPUpgrader{
	EnableCompression: true,
	CheckOrigin: func(ctx *fasthttp.RequestCtx) bool {
		origins := []string{"http://127.0.0.1:8080", "http://127.0.0.1:5469", "https://sandwich.welcomer.gg"}
		origin := gotils.B2S(ctx.Request.Header.Peek("Origin"))

		return gotils.StringSliceInclude(origins, origin)
	},
}

func passFastHTTPResponse(ctx *fasthttp.RequestCtx, data interface{}, success bool, status int) {
	var resp []byte

	var err error

	if success {
		resp, err = json.Marshal(structs.BaseResponse{
			Success: true,
			Data:    data,
		})
	} else {
		resp, err = json.Marshal(structs.BaseResponse{
			Success: false,
			Error:   data.(string),
		})
	}

	if err != nil {
		resp, _ = json.Marshal(structs.BaseResponse{
			Success: false,
			Error:   err.Error(),
		})

		ctx.Error(gotils.B2S(resp), http.StatusInternalServerError)

		return
	}

	if success {
		ctx.SetStatusCode(status)
		_, err = ctx.Write(resp)

		if err != nil {
			ctx.Error(err.Error(), http.StatusInternalServerError)
		}
	} else {
		ctx.Error(gotils.B2S(resp), status)
	}
}

func passResponse(rw http.ResponseWriter, data interface{}, success bool, status int) {
	var resp []byte

	var err error

	if success {
		resp, err = json.Marshal(structs.BaseResponse{
			Success: true,
			Data:    data,
		})
	} else {
		resp, err = json.Marshal(structs.BaseResponse{
			Success: false,
			Error:   data.(string),
		})
	}

	if err != nil {
		resp, _ = json.Marshal(structs.BaseResponse{
			Success: false,
			Error:   err.Error(),
		})
		http.Error(rw, gotils.B2S(resp), http.StatusInternalServerError)

		return
	}

	if success {
		rw.WriteHeader(status)
		_, err = rw.Write(resp)

		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
		}
	} else {
		http.Error(rw, gotils.B2S(resp), status)
	}
}

// HandleRequest handles incoming HTTP requests.
func (sg *Sandwich) HandleRequest(ctx *fasthttp.RequestCtx) {
	var processingMS int64

	start := time.Now()
	path := gotils.B2S(ctx.Path())

	defer func() {
		var log *zerolog.Event

		statusCode := ctx.Response.StatusCode()

		switch {
		case (statusCode >= 400 && statusCode <= 499):
			log = sg.Logger.Warn()
		case (statusCode >= 500 && statusCode <= 599):
			log = sg.Logger.Error()
		default:
			log = sg.Logger.Info()
		}

		// Suppress /api/poll messages
		if path == "/api/poll" && statusCode == 200 {
			return
		}

		log.Msgf("%s %s %s %d %d %dms",
			ctx.RemoteAddr(),
			ctx.Request.Header.Method(),
			ctx.Request.URI().PathOriginal(),
			statusCode,
			len(ctx.Response.Body()),
			processingMS,
		)
	}()

	switch path {
	case "/api/ws":
		APISubscribe(sg, ctx)

		return
	case "/api/console":
		APIConsole(sg, ctx)

		return
	}

	fasthttp.CompressHandlerBrotliLevel(func(ctx *fasthttp.RequestCtx) {
		fasthttpadaptor.NewFastHTTPHandler(sg.Router)(ctx)
		if ctx.Response.StatusCode() != http.StatusNotFound {
			ctx.SetContentType("application/json;charset=utf8")
		}
		// If there is no URL in router then try serving from the dist
		// folder.
		if ctx.Response.StatusCode() == http.StatusNotFound && path != "/" {
			ctx.Response.Reset()
			sg.distHandler(ctx)
		}
		// If there is no URL in router or in dist then send index.html
		if ctx.Response.StatusCode() == http.StatusNotFound {
			ctx.Response.Reset()
			ctx.SendFile(webRootPath + "/index.html")
		}
	}, fasthttp.CompressBrotliDefaultCompression, fasthttp.CompressDefaultCompression)(ctx)

	processingMS = time.Since(start).Milliseconds()
	ctx.Response.Header.Set("X-Elapsed", strconv.FormatInt(processingMS, 10))
}

// AuthenticateSession verifies the session is valid. We simply store the user object
// in the session. There are 100% better ways to do this but for our case this is
// good enough. If HTTP.Public is enabled, it will not require authentication.
// Please only use this if its on a private IP but regardless, you shouldn't have
// this enabled.
func (sg *Sandwich) AuthenticateSession(session *sessions.Session) (auth bool, user *structs.DiscordUser) {
	userBody, ok := session.Values["user"].([]byte)
	if !ok {
		return false, user
	}

	err := json.Unmarshal(userBody, &user)
	if err != nil {
		sg.Logger.Error().Err(err).Msg("Failed to unmarshal user")

		return false, user
	}

	if sg.Configuration.HTTP.Public {
		return true, user
	}

	for _, userID := range sg.Configuration.ElevatedUsers {
		if userID == user.ID.String() {
			return true, user
		}
	}

	return false, user
}

// SaveSession should be used as a defer when handling requests.
func (sg *Sandwich) SaveSession(s *sessions.Session, r *http.Request, rw http.ResponseWriter) {
	if err := s.Save(r, rw); err != nil {
		sg.Logger.Error().Err(err).Msg("Failed to save session")
	}
}

// LogoutHandler handles clearing a user session.
func LogoutHandler(sg *Sandwich) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		session, _ := sg.Store.Get(r, sessionName)
		defer sg.SaveSession(session, r, rw)

		session.Values = make(map[interface{}]interface{})

		http.Redirect(rw, r, "/", http.StatusTemporaryRedirect)
	}
}

// LoginHandler handles CSRF and AuthCode redirection.
func LoginHandler(sg *Sandwich) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		session, _ := sg.Store.Get(r, sessionName)
		defer sg.SaveSession(session, r, rw)

		// Create a simple CSRF string to verify clients and 500 if we
		// cannot generate one.
		csrfString, err := uuid.GenerateUUID()
		if err != nil {
			http.Error(rw, "Internal server error: "+err.Error(), http.StatusInternalServerError)

			return
		}

		// Store the CSRF in the session then redirect the user to the
		// OAuth page.
		session.Values["oauth_csrf"] = csrfString

		url := sg.Configuration.OAuth.AuthCodeURL(csrfString)
		http.Redirect(rw, r, url, http.StatusTemporaryRedirect)
	}
}

// OAuthCallbackHandler handles authenticating discord OAuth and creating
// a user profile if necessary.
func OAuthCallbackHandler(sg *Sandwich) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		session, _ := sg.Store.Get(r, sessionName)
		defer sg.SaveSession(session, r, rw)

		urlQuery := r.URL.Query()
		ctx := context.Background()

		// Validate the CSRF in the session and in the HTTP request.
		// If there is no CSRF in the session it is likely our fault :)
		_csrfString := urlQuery.Get("state")
		csrfString, ok := session.Values["oauth_csrf"].(string)

		if !ok {
			// http.Error(rw, "Missing CSRF state", http.StatusInternalServerError)
			http.Redirect(rw, r, "/login", http.StatusTemporaryRedirect)

			return
		}

		if _csrfString != csrfString {
			// http.Error(rw, "Mismatched CSRF states", http.StatusUnauthorized)
			http.Redirect(rw, r, "/login", http.StatusTemporaryRedirect)

			return
		}

		// Just to be sure, remove the CSRF after we have compared the CSRF
		delete(session.Values, "oauth_csrf")

		// Create an OAuth exchange with the code we were given.
		code := urlQuery.Get("code")

		token, err := sg.Configuration.OAuth.Exchange(ctx, code)
		if err != nil {
			// http.Error(rw, "Failed to exchange code: "+err.Error(), http.StatusInternalServerError)
			http.Redirect(rw, r, "/login", http.StatusTemporaryRedirect)

			return
		}

		// Create a client with our exchanged token and retrieve a user.
		client := sg.Configuration.OAuth.Client(ctx, token)

		resp, err := client.Get(discordUsersMe) //nolint:noctx
		if err != nil {
			http.Redirect(rw, r, "/login", http.StatusTemporaryRedirect)

			return
		}

		body, err := ioutil.ReadAll(resp.Body)
		resp.Body.Close()

		if err != nil {
			// http.Error(rw, err.Error(), http.StatusInternalServerError)
			http.Redirect(rw, r, "/login", http.StatusTemporaryRedirect)

			return
		}

		discordUserResponse := &structs.DiscordUser{}

		if err = json.Unmarshal(body, &discordUserResponse); err != nil {
			// http.Error(rw, err.Error(), http.StatusInternalServerError)
			http.Redirect(rw, r, "/login", http.StatusTemporaryRedirect)

			return
		}

		session.Values["user"] = body

		// Once the user has logged in, send them back to the home page.
		http.Redirect(rw, r, "/", http.StatusTemporaryRedirect)
	}
}

// APIMeHandler handles the /api/me request which returns the user
// object and if they are elevated for the dashboard.
func APIMeHandler(sg *Sandwich) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		session, _ := sg.Store.Get(r, sessionName)
		defer sg.SaveSession(session, r, rw)

		// Authenticate the user
		auth, user := sg.AuthenticateSession(session)

		passResponse(rw, structs.APIMe{
			Authenticated: auth,
			User:          user,
		}, true, http.StatusOK)
	}
}

// APIStatusHandler handles the /api/status request which does not
// require elevation and provides basic information.
func APIStatusHandler(sg *Sandwich) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		now := time.Now().UTC()

		_result := structs.APIStatusResult{
			Managers: make([]structs.APIStatusManager, 0, len(sg.Managers)),
			Uptime:   now.Sub(sg.Start).Round(time.Millisecond).Milliseconds(),
		}

		for _, manager := range sg.Managers {
			_manager := structs.APIStatusManager{
				DisplayName: manager.Configuration.DisplayName,
				Guilds:      0,
				ShardGroups: make([]structs.APIStatusShardGroup, 0, len(manager.ShardGroups)),
			}

			for _, shardgroup := range manager.ShardGroups {
				_manager.Guilds += int64(shardgroup.GetGuildCount())

				shardgroup.StatusMu.RLock()
				_shardgroup := structs.APIStatusShardGroup{
					ID:     shardgroup.ID,
					Status: shardgroup.Status,
					Shards: make([]structs.APIStatusShard, 0, len(shardgroup.Shards)),
				}
				shardgroup.StatusMu.RUnlock()

				shardgroup.ShardsMu.RLock()
				for _, shard := range shardgroup.Shards {
					shard.StatusMu.RLock()
					_shard := structs.APIStatusShard{
						Status:  shard.Status,
						Latency: shard.Latency(),
						Uptime:  now.Sub(shard.Start).Round(time.Millisecond).Milliseconds(),
					}
					shard.StatusMu.RUnlock()

					_shardgroup.Shards = append(_shardgroup.Shards, _shard)
				}
				shardgroup.ShardsMu.RUnlock()

				_manager.ShardGroups = append(_manager.ShardGroups, _shardgroup)
			}

			_result.Managers = append(_result.Managers, _manager)
		}

		passResponse(rw, _result, true, http.StatusOK)
	}
}

// ConstructAnalytics returns a LineChart struct based off of manager analytics.
func (sg *Sandwich) ConstructAnalytics() structs.LineChart {
	datasets := make([]structs.Dataset, 0, len(sg.Managers))

	// Create and sort x axis keys.
	mankeys := make([]string, 0, len(sg.Managers))
	for key := range sg.Managers {
		mankeys = append(mankeys, key)
	}

	sort.Strings(mankeys)

	for i, ident := range mankeys {
		mg := sg.Managers[ident]

		mg.AnalyticsMu.RLock()
		if mg.Analytics == nil {
			mg.AnalyticsMu.RUnlock()

			continue
		}

		mg.Analytics.RLock()
		data := make([]interface{}, 0, len(mg.Analytics.Samples))

		for _, sample := range mg.Analytics.Samples {
			data = append(data, structs.DataStamp{Time: sample.StoredAt, Value: sample.Value})
		}
		mg.Analytics.RUnlock()
		mg.AnalyticsMu.RUnlock()

		colour := structs.LineChartColours[i%len(structs.LineChartColours)]

		datasets = append(datasets, structs.Dataset{
			Label:            mg.Configuration.DisplayName,
			BackgroundColour: colour[0],
			BorderColour:     colour[1],
			Data:             data,
		})
	}

	return structs.LineChart{
		Datasets: datasets,
	}
}

// APIAnalyticsHandler handles the /api/analytics request which
// requires elevation.
func APIAnalyticsHandler(sg *Sandwich) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		session, _ := sg.Store.Get(r, sessionName)
		if auth, _ := sg.AuthenticateSession(session); !auth {
			passResponse(rw, forbiddenMessage, false, http.StatusForbidden)

			return
		}

		passResponse(rw, sg.FetchAnalytics(), true, http.StatusOK)
	}
}

// FetchAnalytics returns the data for the /api/analytics endpoint.
func (sg *Sandwich) FetchAnalytics() (result structs.APIAnalyticsResult) {
	managers := make([]structs.ManagerInformation, 0, len(sg.Managers))
	guildCount := int64(0)

	sg.State.ChannelsMu.RLock()
	channelCount := int64(len(sg.State.Channels))
	sg.State.ChannelsMu.RUnlock()

	sg.State.UsersMu.RLock()
	userCount := int64(len(sg.State.Users))
	sg.State.UsersMu.RUnlock()

	sg.State.EmojisMu.RLock()
	emojiCount := int64(len(sg.State.Emojis))
	sg.State.EmojisMu.RUnlock()

	memberCount := int64(0)

	sg.State.GuildMembersMu.RLock()
	for _, gm := range sg.State.GuildMembers {
		gm.MembersMu.RLock()
		memberCount += int64(len(gm.Members))
		gm.MembersMu.RUnlock()
	}
	sg.State.GuildMembersMu.RUnlock()

	for _, manager := range sg.Managers {
		manager.ConfigurationMu.RLock()

		managerGuilds := int64(0)
		statuses := make(map[int32]structs.ShardGroupStatus)

		manager.ShardGroupsMu.RLock()
		for i, sg := range manager.ShardGroups {
			sg.StatusMu.RLock()
			statuses[i] = sg.Status
			sg.StatusMu.RUnlock()

			sg.GuildsMu.RLock()
			managerGuilds += int64(len(sg.Guilds))
			sg.GuildsMu.RUnlock()
		}
		manager.ShardGroupsMu.RUnlock()

		_manager := structs.ManagerInformation{
			Name:      manager.Configuration.DisplayName,
			Guilds:    managerGuilds,
			Status:    statuses,
			AutoStart: manager.Configuration.AutoStart,
		}
		manager.ConfigurationMu.RUnlock()

		guildCount += managerGuilds

		managers = append(managers, _manager)
	}

	now := time.Now()

	result = structs.APIAnalyticsResult{
		Graph:  sg.ConstructAnalytics(),
		Guilds: guildCount,

		Channels: channelCount,
		Users:    userCount,
		Members:  memberCount,
		Emojis:   emojiCount,

		Uptime:   DurationTimestamp(now.Sub(sg.Start)),
		Events:   atomic.LoadInt64(sg.TotalEvents),
		Managers: managers,
	}

	return result
}

// APIPollHandler is the HTTP REST equivalent to the /api/ws endpoint
// and is likely to be used as it supports compression.
func APIPollHandler(sg *Sandwich) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		session, _ := sg.Store.Get(r, sessionName)
		if auth, _ := sg.AuthenticateSession(session); !auth {
			passResponse(rw, forbiddenMessage, false, http.StatusForbidden)

			return
		}

		resttunnel, _, _, _, _ := sg.FetchRestTunnelResponse() //nolint:bodyclose

		passResponse(rw, structs.APISubscribeResult{
			Managers:          sg.FetchManagerResponse(),
			RestTunnel:        resttunnel,
			Analytics:         sg.FetchAnalytics(),
			Start:             sg.Start,
			RestTunnelEnabled: sg.RestTunnelEnabled.IsSet(),
			Waiting:           atomic.LoadInt64(sg.PoolWaiting),
		}, true, http.StatusOK)
	}
}

// APIConsole is a websocket that relays the stdout to clients.
func APIConsole(sg *Sandwich, ctx *fasthttp.RequestCtx) {
	fasthttpadaptor.NewFastHTTPHandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		session, _ := sg.Store.Get(r, sessionName)
		if auth, _ := sg.AuthenticateSession(session); !auth {
			passResponse(rw, forbiddenMessage, false, http.StatusForbidden)

			return
		}
		rw.WriteHeader(http.StatusOK)
	})(ctx)

	if ctx.Response.StatusCode() != http.StatusOK {
		return
	}

	err := upgrader.Upgrade(ctx, func(conn *websocket.Conn) {
		conn.EnableWriteCompression(true)
		if err := conn.SetCompressionLevel(flate.BestCompression); err != nil {
			sg.Logger.Error().Err(err).Msg("Failed to set compression level")
		}

		id := sg.ConsolePump.RegisterConnection(conn)
		defer sg.ConsolePump.DeregisterConnection(id)

		for {
			msgType, _, _ := conn.ReadMessage()
			if msgType == -1 {
				return
			}
		}
	})
	if err != nil {
		sg.Logger.Error().Err(err).Msg("Failed to upgrade connection")
		passFastHTTPResponse(ctx, err.Error(), false, http.StatusInternalServerError)

		return
	}
}

// APISubscribe is a websocket that incorporates the /api/managers,
// /api/resttunnel and /api/configuration endpoint.
func APISubscribe(sg *Sandwich, ctx *fasthttp.RequestCtx) {
	fasthttpadaptor.NewFastHTTPHandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		session, _ := sg.Store.Get(r, sessionName)
		if auth, _ := sg.AuthenticateSession(session); !auth {
			passResponse(rw, forbiddenMessage, false, http.StatusForbidden)

			return
		}
		rw.WriteHeader(http.StatusOK)
	})(ctx)

	if ctx.Response.StatusCode() != http.StatusOK {
		return
	}

	err := upgrader.Upgrade(ctx, func(conn *websocket.Conn) {
		conn.EnableWriteCompression(true)
		if err := conn.SetCompressionLevel(flate.BestCompression); err != nil {
			sg.Logger.Error().Err(err).Msg("Failed to set compression level")
		}

		t := time.NewTicker(time.Second * apiSubscribeDuration)
		for {
			result := structs.APISubscribeResult{}
			result.Managers = sg.FetchManagerResponse()
			result.Analytics = sg.FetchAnalytics()

			resttunnel, _, _, _, _ := sg.FetchRestTunnelResponse() //nolint:bodyclose
			if len(resttunnel) > 0 {
				result.RestTunnel = resttunnel
			}

			resp, err := json.Marshal(result)
			if err != nil {
				sg.Logger.Warn().Err(err).Msg("Failed to marshal websocket payload")
			}

			err = conn.WriteMessage(websocket.TextMessage, resp)
			if err != nil {
				break
			}
			<-t.C
		}
	})
	if err != nil {
		sg.Logger.Error().Err(err).Msg("Failed to upgrade APISubscribe connection")
		passFastHTTPResponse(ctx, err.Error(), false, http.StatusInternalServerError)

		return
	}
}

// APIManagersHandler handles the /api/managers endpoint.
func APIManagersHandler(sg *Sandwich) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		session, _ := sg.Store.Get(r, sessionName)
		if auth, _ := sg.AuthenticateSession(session); !auth {
			passResponse(rw, forbiddenMessage, false, http.StatusForbidden)

			return
		}

		passResponse(rw, sg.FetchManagerResponse(), true, http.StatusOK)
	}
}

// FetchManagerResponse returns the data for the /api/manager endpoint.
func (sg *Sandwich) FetchManagerResponse() (managers map[string]structs.APIConfigurationResponseManager) {
	managers = make(map[string]structs.APIConfigurationResponseManager)

	sg.ManagersMu.RLock()
	for managerID, manager := range sg.Managers {
		mg := structs.APIConfigurationResponseManager{}

		manager.ConfigurationMu.RLock()
		mg.Configuration = manager.Configuration
		manager.ConfigurationMu.RUnlock()

		manager.GatewayMu.RLock()
		mg.Gateway = manager.Gateway
		manager.GatewayMu.RUnlock()

		manager.ErrorMu.RLock()
		mg.Error = manager.Error
		manager.ErrorMu.RUnlock()

		mg.ShardGroups = make(map[int32]structs.APIConfigurationResponseShardGroup)

		manager.ShardGroupsMu.RLock()
		for shardgroupID, shardgroup := range manager.ShardGroups {
			shg := structs.APIConfigurationResponseShardGroup{
				Start:      shardgroup.Start,
				ID:         shardgroup.ID,
				ShardCount: shardgroup.ShardCount,
				ShardIDs:   shardgroup.ShardIDs,
				WaitingFor: atomic.LoadInt32(shardgroup.WaitingFor),
			}

			shardgroup.StatusMu.RLock()
			shg.Status = shardgroup.Status
			shardgroup.StatusMu.RUnlock()

			shardgroup.ErrorMu.RLock()
			shg.Error = shardgroup.Error
			shardgroup.ErrorMu.RUnlock()

			shg.Shards = make(map[int]interface{})

			shardgroup.ShardsMu.RLock()
			for shardID, shard := range shardgroup.Shards {
				shard.RLock()
				shd := structs.APIConfigurationResponseShard{
					ShardID:              shard.ShardID,
					User:                 shard.User,
					HeartbeatInterval:    shard.HeartbeatInterval,
					MaxHeartbeatFailures: shard.MaxHeartbeatFailures,
					Start:                shard.Start,
					Retries:              atomic.LoadInt32(shard.Retries),
				}
				shard.RUnlock()

				shard.StatusMu.RLock()
				shd.Status = shard.Status
				shard.StatusMu.RUnlock()

				shard.LastHeartbeatMu.RLock()
				shd.LastHeartbeatAck = shard.LastHeartbeatAck
				shd.LastHeartbeatSent = shard.LastHeartbeatSent
				shard.LastHeartbeatMu.RUnlock()

				shg.Shards[shardID] = shd
			}
			shardgroup.ShardsMu.RUnlock()

			mg.ShardGroups[shardgroupID] = shg
		}
		manager.ShardGroupsMu.RUnlock()

		managers[managerID] = mg
	}
	sg.ManagersMu.RUnlock()

	return managers
}

// APIConfigurationHandler handles the /api/configuration endpoint.
func APIConfigurationHandler(sg *Sandwich) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		session, _ := sg.Store.Get(r, sessionName)
		if auth, _ := sg.AuthenticateSession(session); !auth {
			passResponse(rw, forbiddenMessage, false, http.StatusForbidden)

			return
		}

		passResponse(rw, sg.FetchConfigurationResponse(), true, http.StatusOK)
	}
}

// FetchConfigurationResponse returns the data for the /api/configuration endpoint.
func (sg *Sandwich) FetchConfigurationResponse() (pl structs.APIConfigurationResponse) {
	pl = structs.APIConfigurationResponse{
		Start:             sg.Start,
		RestTunnelEnabled: sg.RestTunnelEnabled.IsSet(),
		MQDrivers:         mqclients.MQClients,
		Version:           VERSION,
	}

	sg.ConfigurationMu.RLock()
	pl.Configuration = sg.Configuration
	sg.ConfigurationMu.RUnlock()

	return
}

// APIRestTunnelHandler handles the /api/resttunnel endpoint.
func APIRestTunnelHandler(sg *Sandwich) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		session, _ := sg.Store.Get(r, sessionName)
		if auth, _ := sg.AuthenticateSession(session); !auth {
			passResponse(rw, forbiddenMessage, false, http.StatusForbidden)

			return
		}

		body, resp, err, ok, status := sg.FetchRestTunnelResponse() //nolint:bodyclose
		if err != "" || !ok {
			passResponse(rw, err, ok, status)

			return
		}

		// We want to write directly as its a proxied request.
		rw.WriteHeader(resp.StatusCode)

		if _, err := rw.Write(body); err != nil {
			passResponse(rw, err.Error(), false, http.StatusInternalServerError)
		}
	}
}

// FetchRestTunnelResponse returns the raw body for the /api/resttunnel request.
func (sg *Sandwich) FetchRestTunnelResponse() (body []byte, resp *http.Response, err string, ok bool, status int) {
	if sg.RestTunnelEnabled.IsNotSet() {
		err = "RestTunnel is not enabled"
		status = http.StatusOK
		ok = true

		return
	}

	_url, _err := url.Parse(sg.Configuration.RestTunnel.URL)

	if _err != nil {
		err = _err.Error()
		status = http.StatusInternalServerError

		return
	}

	resp, _err = http.Get(_url.Scheme + "://" + _url.Host + "/resttunnel/analytics") //nolint:noctx

	if _err != nil {
		err = _err.Error()
		status = http.StatusInternalServerError

		return
	}

	body, _err = ioutil.ReadAll(resp.Body)
	resp.Body.Close()

	if _err != nil {
		err = _err.Error()
		status = http.StatusInternalServerError

		return
	}

	return body, resp, err, true, http.StatusOK
}

// APIRPCHandler handles the /api/rpc endpoint.
func APIRPCHandler(sg *Sandwich) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		session, _ := sg.Store.Get(r, sessionName)

		auth, user := sg.AuthenticateSession(session)
		if !auth {
			passResponse(rw, forbiddenMessage, false, http.StatusForbidden)

			return
		}

		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			passResponse(rw, err.Error(), false, http.StatusInternalServerError)

			return
		}

		RPCMessage := structs.RPCRequest{}
		err = json.Unmarshal(body, &RPCMessage)

		if err != nil {
			passResponse(rw, "Invalid payload sent", false, http.StatusBadRequest)

			return
		}

		ok := executeRequest(sg, user, RPCMessage, rw)
		if !ok {
			passResponse(rw, fmt.Sprintf("Unknown method: %s", RPCMessage.Method), false, http.StatusBadRequest)

			return
		}
	}
}

func createEndpoints(sg *Sandwich) (router *methodrouter.MethodRouter) {
	router = methodrouter.NewMethodRouter()

	router.HandleFunc("/login", LoginHandler(sg), "GET")
	router.HandleFunc("/logout", LogoutHandler(sg), "GET")
	router.HandleFunc("/oauth2/callback", OAuthCallbackHandler(sg), "GET")

	router.HandleFunc("/api/me", APIMeHandler(sg), "GET")

	router.HandleFunc("/api/status", APIStatusHandler(sg), "GET")

	router.HandleFunc("/api/analytics", APIAnalyticsHandler(sg), "GET")
	router.HandleFunc("/api/managers", APIManagersHandler(sg), "GET")
	router.HandleFunc("/api/configuration", APIConfigurationHandler(sg), "GET")
	router.HandleFunc("/api/resttunnel", APIRestTunnelHandler(sg), "GET")

	router.HandleFunc("/api/poll", APIPollHandler(sg), "GET")
	router.HandleFunc("/api/rpc", APIRPCHandler(sg), "POST")

	return
}
