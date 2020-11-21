package gateway

import (
	"fmt"
	"math/rand"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/TheRockettek/Sandwich-Daemon/structs"
	"github.com/nats-io/stan.go"
)

var rpcHandlers = make(map[string]func(sg *Sandwich, req structs.RPCRequest, rw http.ResponseWriter) bool)

func registerHandler(method string, f func(sg *Sandwich, req structs.RPCRequest, rw http.ResponseWriter) bool) {
	rpcHandlers[method] = f
}

func executeRequest(sg *Sandwich, req structs.RPCRequest, rw http.ResponseWriter) (ok bool) {
	if f, ok := rpcHandlers[req.Method]; ok {
		f(sg, req, rw)
		return true
	}
	return false
}

// RPCManagerShardGroupCreate handles the creation of a new shardgroup
func RPCManagerShardGroupCreate(sg *Sandwich, req structs.RPCRequest, rw http.ResponseWriter) bool {
	event := structs.RPCManagerShardGroupCreateEvent{}
	err := json.Unmarshal(req.Data, &event)
	if err != nil {
		passResponse(rw, err.Error(), false, http.StatusBadRequest)
		return false
	}

	// Verify the cluster exists
	sg.ManagersMu.RLock()
	manager, ok := sg.Managers[event.Manager]
	sg.ManagersMu.RUnlock()
	if !ok {
		passResponse(rw, "Invalid manager provided", false, http.StatusBadRequest)
		return false
	}

	if event.AutoShard {
		manager.GatewayMu.Lock()
		gw, err := manager.GetGateway()
		if err != nil {
			manager.Logger.Warn().Err(err).Msg("Received error retrieving gateway object. Using old response.")
		} else {
			// We will only overwrite the gateway if it does not error as we
			// will just recycle the old response.
			manager.Gateway = gw
		}
		event.ShardCount = manager.Gateway.Shards
		manager.GatewayMu.Unlock()
	}
	if event.ShardCount < 1 {
		sg.Logger.Debug().Msg("Set ShardCount to 1 as it was less than 1")
		event.ShardCount = 1
	}

	if event.AutoIDs {
		event.ShardIDs = manager.GenerateShardIDs(event.ShardCount)
	} else {
		event.ShardIDs = ReturnRange(event.RawShardIDs, event.ShardCount)
	}

	sg.Logger.Debug().Msgf("Created ShardIDs: %v", event.ShardIDs)

	if len(event.ShardIDs) == 0 {
		sg.Logger.Debug().Msg("Set ShardIDs to [0] as it was empty")
		event.ShardIDs = []int{0}
	}

	if len(event.ShardIDs) > event.ShardCount {
		sg.Logger.Warn().Msgf("Length of ShardIDs is larger than the ShardCount %d > %d", len(event.ShardIDs), event.ShardCount)
		// TODO: We should handle this properly but it will error out when it starts up anyway
	}

	if len(event.ShardIDs) < manager.Gateway.SessionStartLimit.Remaining {
		manager.Scale(event.ShardIDs, event.ShardCount, true)
		passResponse(rw, true, true, http.StatusOK)
	} else {
		passResponse(rw, fmt.Sprintf("Not enough sessions to start %d shard(s). %d remain", len(event.ShardIDs), manager.Gateway.SessionStartLimit.Remaining), false, http.StatusBadRequest)
	}

	return true
}

// RPCManagerShardGroupStop handles stopping a shardgroup
func RPCManagerShardGroupStop(sg *Sandwich, req structs.RPCRequest, rw http.ResponseWriter) bool {
	event := structs.RPCManagerShardGroupStopEvent{}
	err := json.Unmarshal(req.Data, &event)
	if err != nil {
		passResponse(rw, err.Error(), false, http.StatusBadRequest)
		return false
	}

	sg.ManagersMu.RLock()
	manager, ok := sg.Managers[event.Manager]
	sg.ManagersMu.RUnlock()
	if !ok {
		passResponse(rw, "Invalid manager provided", false, http.StatusBadRequest)
		return false
	}

	manager.ShardGroupsMu.RLock()
	shardgroup, ok := manager.ShardGroups[event.ShardGroup]
	manager.ShardGroupsMu.RUnlock()
	if !ok {
		passResponse(rw, "Invalid shardgroup provided", false, http.StatusBadRequest)
		return false
	}

	shardgroup.Close()
	passResponse(rw, true, true, http.StatusOK)
	return true
}

// RPCManagerShardGroupDelete handles deleting a shardgroup
func RPCManagerShardGroupDelete(sg *Sandwich, req structs.RPCRequest, rw http.ResponseWriter) bool {
	event := structs.RPCManagerShardGroupDeleteEvent{}
	err := json.Unmarshal(req.Data, &event)
	if err != nil {
		passResponse(rw, err.Error(), false, http.StatusBadRequest)
		return false
	}

	sg.ManagersMu.RLock()
	manager, ok := sg.Managers[event.Manager]
	sg.ManagersMu.RUnlock()
	if !ok {
		passResponse(rw, "Invalid manager provided", false, http.StatusBadRequest)
		return false
	}

	manager.ShardGroupsMu.RLock()
	shardgroup, ok := manager.ShardGroups[event.ShardGroup]
	manager.ShardGroupsMu.RUnlock()
	if !ok {
		passResponse(rw, "Invalid shardgroup provided", false, http.StatusBadRequest)
		return false
	}

	if shardgroup.Status != structs.ShardGroupClosed {
		passResponse(rw, "ShardGroup is not closed", false, http.StatusBadRequest)
		return false
	}

	manager.ShardGroupsMu.Lock()
	delete(manager.ShardGroups, event.ShardGroup)
	manager.ShardGroupsMu.Unlock()

	passResponse(rw, true, true, http.StatusOK)
	return true
}

// RPCManagerUpdate handles updating a managers configuration
func RPCManagerUpdate(sg *Sandwich, req structs.RPCRequest, rw http.ResponseWriter) bool {
	event := ManagerConfiguration{}
	err := json.Unmarshal(req.Data, &event)
	if err != nil {
		passResponse(rw, err.Error(), false, http.StatusBadRequest)
		return false
	}

	sg.ManagersMu.RLock()
	manager, ok := sg.Managers[event.Identifier]
	sg.ManagersMu.RUnlock()
	if !ok {
		passResponse(rw, "Invalid manager provided", false, http.StatusBadRequest)
		return false
	}

	sg.ConfigurationMu.Lock()
	manager.ConfigurationMu.Lock()
	defer sg.ConfigurationMu.Unlock()
	defer manager.ConfigurationMu.Unlock()

	if event.Messaging.UseRandomSuffix != manager.Configuration.Messaging.UseRandomSuffix {
		var clientName string
		if manager.Configuration.Messaging.UseRandomSuffix {
			clientName = manager.Configuration.Messaging.ClientName + "-" + strconv.Itoa(rand.Intn(9999))
		} else {
			clientName = manager.Configuration.Messaging.ClientName
		}

		stanClient, err := stan.Connect(
			manager.Sandwich.Configuration.NATS.Cluster,
			clientName,
			stan.NatsConn(manager.NatsClient),
		)

		if err == nil {
			manager.StanClient = stanClient
		}
	}

	if !reflect.DeepEqual(event.Events.EventBlacklist, manager.Configuration.Events.EventBlacklist) {
		manager.EventBlacklist = manager.Configuration.Events.EventBlacklist
	}

	if !reflect.DeepEqual(event.Events.ProduceBlacklist, manager.Configuration.Events.ProduceBlacklist) {
		manager.ProduceBlacklist = manager.Configuration.Events.ProduceBlacklist
	}

	manager.Configuration = &event
	manager.Client.Token = manager.Configuration.Token

	// Updates the managers in the sandwich configuration
	managers := []*ManagerConfiguration{}
	for _, _manager := range sg.Configuration.Managers {
		if _manager.Identifier == manager.Configuration.Identifier {
			managers = append(managers, manager.Configuration)
		} else {
			managers = append(managers, _manager)
		}
	}
	sg.Configuration.Managers = managers

	err = sg.SaveConfiguration(sg.Configuration, ConfigurationPath)

	if err != nil {
		passResponse(rw, err.Error(), false, http.StatusInternalServerError)
		return false
	}

	passResponse(rw, true, true, http.StatusOK)
	return true
}

// RPCManagerCreate handles the creation of new managers
func RPCManagerCreate(sg *Sandwich, req structs.RPCRequest, rw http.ResponseWriter) bool {
	event := structs.RPCManagerCreateEvent{}
	err := json.Unmarshal(req.Data, &event)
	if err != nil {
		passResponse(rw, err.Error(), false, http.StatusBadRequest)
		return false
	}

	_displayName := event.Identifier
	_identifier := strings.ReplaceAll(event.Identifier, " ", "")

	sg.ManagersMu.RLock()
	_, ok := sg.Managers[_identifier]
	sg.ManagersMu.RUnlock()
	if ok {
		passResponse(rw, "Manager with this name already exists", false, http.StatusBadRequest)
		return false
	}

	config := &ManagerConfiguration{
		Persist:     event.Persist,
		Identifier:  _identifier,
		DisplayName: _displayName,
		Token:       event.Token,
	}

	// Default configuration things
	config.Caching.RedisPrefix = event.Prefix
	config.Messaging.ClientName = event.Client
	config.Messaging.ChannelName = event.Channel
	config.Bot.DefaultPresence = &structs.UpdateStatus{}

	config.Messaging.UseRandomSuffix = true
	config.Bot.Retries = 2
	config.Bot.Intents = 32511
	config.Bot.Compression = true
	config.Bot.LargeThreshold = 250
	config.Sharding.ShardCount = 1
	config.Bot.MaxHeartbeatFailures = 5

	sg.ConfigurationMu.Lock()
	sg.Configuration.Managers = append(sg.Configuration.Managers, config)
	sg.ConfigurationMu.Unlock()

	sg.ConfigurationMu.RLock()
	err = sg.SaveConfiguration(sg.Configuration, ConfigurationPath)
	sg.ConfigurationMu.RUnlock()

	if err != nil {
		passResponse(rw, err.Error(), false, http.StatusInternalServerError)
		return false
	}

	manager, err := sg.NewManager(config)
	if err != nil {
		passResponse(rw, err.Error(), false, http.StatusInternalServerError)
		return false
	}

	sg.ManagersMu.Lock()
	sg.Managers[config.Identifier] = manager
	sg.ManagersMu.Unlock()

	gw, err := manager.GetGateway()
	if err != nil {
		passResponse(rw, err.Error(), false, http.StatusInternalServerError)
		return false
	}

	manager.GatewayMu.Lock()
	manager.Gateway = gw
	manager.GatewayMu.Unlock()

	err = manager.Open()
	if err != nil {
		passResponse(rw, err.Error(), false, http.StatusInternalServerError)
		return false
	}

	passResponse(rw, true, true, http.StatusOK)
	return true
}

// RPCManagerDelete handles deleting managers
func RPCManagerDelete(sg *Sandwich, req structs.RPCRequest, rw http.ResponseWriter) bool {
	event := structs.RPCManagerDeleteEvent{}
	err := json.Unmarshal(req.Data, &event)
	if err != nil {
		passResponse(rw, err.Error(), false, http.StatusBadRequest)
		return false
	}

	if event.Confirm != event.Manager {
		passResponse(rw, "Incorrect confirm value. Must be equal to manager", false, http.StatusBadRequest)
		return false
	}

	sg.ManagersMu.RLock()
	manager, ok := sg.Managers[event.Manager]
	sg.ManagersMu.RUnlock()
	if !ok {
		passResponse(rw, "Invalid manager provided", false, http.StatusBadRequest)
		return false
	}

	manager.Close()

	sg.ManagersMu.Lock()
	delete(sg.Managers, event.Manager)
	sg.ManagersMu.Unlock()

	managers := []*ManagerConfiguration{}
	sg.ConfigurationMu.Lock()
	for _, _manager := range sg.Configuration.Managers {
		if _manager.Identifier != event.Manager {
			managers = append(managers, _manager)
		}
	}
	sg.Configuration.Managers = managers

	err = sg.SaveConfiguration(sg.Configuration, ConfigurationPath)
	sg.ConfigurationMu.Unlock()

	if err != nil {
		passResponse(rw, err.Error(), false, http.StatusInternalServerError)
		return false
	}

	passResponse(rw, true, true, http.StatusOK)
	return true
}

// RPCManagerRestart handles restarting a manager
func RPCManagerRestart(sg *Sandwich, req structs.RPCRequest, rw http.ResponseWriter) bool {
	event := structs.RPCManagerRestartEvent{}
	err := json.Unmarshal(req.Data, &event)
	if err != nil {
		passResponse(rw, err.Error(), false, http.StatusBadRequest)
		return false
	}

	if event.Confirm != event.Manager {
		passResponse(rw, "Incorrect confirm value. Must be equal to manager", false, http.StatusBadRequest)
		return false
	}

	sg.ManagersMu.RLock()
	manager, ok := sg.Managers[event.Manager]
	sg.ManagersMu.RUnlock()
	if !ok {
		passResponse(rw, "Invalid manager provided", false, http.StatusBadRequest)
		return false
	}

	manager.Close()

	sg.ManagersMu.Lock()
	delete(sg.Managers, event.Manager)
	sg.ManagersMu.Unlock()

	manager, err = sg.NewManager(manager.Configuration)
	if err != nil {
		passResponse(rw, err.Error(), false, http.StatusInternalServerError)
		return false
	}

	sg.ManagersMu.Lock()
	sg.Managers[event.Manager] = manager
	sg.ManagersMu.Unlock()

	gw, err := manager.GetGateway()
	if err != nil {
		passResponse(rw, err.Error(), false, http.StatusInternalServerError)
		return false
	}

	manager.GatewayMu.Lock()
	manager.Gateway = gw
	manager.GatewayMu.Unlock()

	passResponse(rw, true, true, http.StatusOK)
	return true
}

// RPCManagerRefreshGateway handles refreshing the gateway
func RPCManagerRefreshGateway(sg *Sandwich, req structs.RPCRequest, rw http.ResponseWriter) bool {
	event := structs.RPCManagerRefreshGatewayEvent{}
	err := json.Unmarshal(req.Data, &event)
	if err != nil {
		passResponse(rw, err.Error(), false, http.StatusBadRequest)
		return false
	}

	sg.ManagersMu.RLock()
	manager, ok := sg.Managers[event.Manager]
	sg.ManagersMu.RUnlock()
	if !ok {
		passResponse(rw, "Invalid manager provided", false, http.StatusBadRequest)
		return false
	}

	gw, err := manager.GetGateway()
	if err != nil {
		passResponse(rw, err.Error(), false, http.StatusInternalServerError)
		return false
	}

	manager.GatewayMu.Lock()
	manager.Gateway = gw
	manager.GatewayMu.Unlock()

	passResponse(rw, true, true, http.StatusOK)
	return true
}

// RPCDaemonVerifyRestTunnel checks if RestTunnel is active
func RPCDaemonVerifyRestTunnel(sg *Sandwich, req structs.RPCRequest, rw http.ResponseWriter) bool {
	var restTunnelEnabled bool
	var reverse bool
	var err error

	sg.ConfigurationMu.Lock()
	if sg.Configuration.RestTunnel.Enabled {
		restTunnelEnabled, reverse, err = sg.VerifyRestTunnel(sg.Configuration.RestTunnel.URL)
		if err != nil {
			sg.Logger.Error().Err(err).Msg("Failed to verify RestTunnel")
		}
	} else {
		restTunnelEnabled = false
	}
	sg.RestTunnelReverse.SetTo(reverse)
	sg.RestTunnelEnabled.SetTo(restTunnelEnabled)
	sg.ConfigurationMu.Unlock()

	passResponse(rw, restTunnelEnabled, true, http.StatusOK)
	return true
}

// RPCDaemonUpdate updates the daemon settings
func RPCDaemonUpdate(sg *Sandwich, req structs.RPCRequest, rw http.ResponseWriter) bool {
	event := SandwichConfiguration{}
	err := json.Unmarshal(req.Data, &event)
	if err != nil {
		passResponse(rw, err.Error(), false, http.StatusBadRequest)
		return false
	}

	configuration, err := sg.LoadConfiguration(ConfigurationPath)
	if err != nil {
		passResponse(rw, err.Error(), false, http.StatusInternalServerError)
		return false
	}

	event.Managers = configuration.Managers
	err = sg.SaveConfiguration(&event, ConfigurationPath)
	if err != nil {
		sg.Logger.Error().Err(err).Msg("Failed to save configuration however silently continuing")
	}

	var restTunnelEnabled bool
	var reverse bool

	sg.ConfigurationMu.Lock()
	if sg.Configuration.RestTunnel.Enabled {
		restTunnelEnabled, reverse, err = sg.VerifyRestTunnel(sg.Configuration.RestTunnel.URL)
		if err != nil {
			sg.Logger.Error().Err(err).Msg("Failed to verify RestTunnel")
		}
	} else {
		restTunnelEnabled = false
	}

	var restTunnelURL string
	if restTunnelEnabled {
		restTunnelURL = event.RestTunnel.URL
	} else {
		restTunnelURL = ""
	}

	if restTunnelEnabled != sg.RestTunnelEnabled.IsSet() || reverse != sg.RestTunnelReverse.IsSet() {
		sg.ManagersMu.RLock()
		for _, _manager := range sg.Managers {
			_manager.Client.mu.Lock()
			_manager.Client.restTunnelURL = restTunnelURL
			_manager.Client.reverse = reverse
			_manager.Client.mu.Unlock()
		}
		sg.ManagersMu.RUnlock()
	}

	sg.RestTunnelEnabled.SetTo(restTunnelEnabled)
	sg.ConfigurationMu.Unlock()

	event.Managers = sg.Configuration.Managers
	sg.ConfigurationMu.Lock()
	sg.Configuration = &event
	sg.ConfigurationMu.Unlock()

	passResponse(rw, true, true, http.StatusOK)
	return true
}

func init() {
	registerHandler("manager:update", RPCManagerUpdate)
	registerHandler("manager:create", RPCManagerCreate)
	registerHandler("manager:delete", RPCManagerDelete)
	registerHandler("manager:restart", RPCManagerRestart)
	registerHandler("manager:refresh_gateway", RPCManagerRefreshGateway)

	registerHandler("manager:shardgroup:create", RPCManagerShardGroupCreate)
	registerHandler("manager:shardgroup:stop", RPCManagerShardGroupStop)
	registerHandler("manager:shardgroup:delete", RPCManagerShardGroupDelete)

	registerHandler("daemon:verify_resttunnel", RPCDaemonVerifyRestTunnel)
	registerHandler("daemon:update", RPCDaemonUpdate)
}
