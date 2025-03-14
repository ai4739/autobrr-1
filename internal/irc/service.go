package irc

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/autobrr/autobrr/internal/domain"
	"github.com/autobrr/autobrr/internal/filter"
	"github.com/autobrr/autobrr/internal/indexer"
	"github.com/autobrr/autobrr/internal/release"

	"github.com/rs/zerolog/log"
)

type Service interface {
	StartHandlers()
	StopHandlers()
	StopNetwork(key handlerKey) error
	ListNetworks(ctx context.Context) ([]domain.IrcNetwork, error)
	GetNetworksWithHealth(ctx context.Context) ([]domain.IrcNetworkWithHealth, error)
	GetNetworkByID(id int64) (*domain.IrcNetwork, error)
	DeleteNetwork(ctx context.Context, id int64) error
	StoreNetwork(ctx context.Context, network *domain.IrcNetwork) error
	UpdateNetwork(ctx context.Context, network *domain.IrcNetwork) error
	StoreChannel(networkID int64, channel *domain.IrcChannel) error
}

type service struct {
	repo           domain.IrcRepo
	filterService  filter.Service
	indexerService indexer.Service
	releaseService release.Service
	indexerMap     map[string]string
	handlers       map[handlerKey]*Handler

	stopWG sync.WaitGroup
	lock   sync.Mutex
}

func NewService(repo domain.IrcRepo, filterService filter.Service, indexerSvc indexer.Service, releaseSvc release.Service) Service {
	return &service{
		repo:           repo,
		filterService:  filterService,
		indexerService: indexerSvc,
		releaseService: releaseSvc,
		handlers:       make(map[handlerKey]*Handler),
	}
}

type handlerKey struct {
	server string
	nick   string
}

func (s *service) StartHandlers() {
	networks, err := s.repo.FindActiveNetworks(context.Background())
	if err != nil {
		log.Error().Msgf("failed to list networks: %v", err)
	}

	for _, network := range networks {
		if !network.Enabled {
			continue
		}

		// check if already in handlers
		//v, ok := s.handlers[network.Name]

		s.lock.Lock()
		channels, err := s.repo.ListChannels(network.ID)
		if err != nil {
			log.Error().Err(err).Msgf("failed to list channels for network %q", network.Server)
		}
		network.Channels = channels

		// find indexer definitions for network and add
		definitions := s.indexerService.GetIndexersByIRCNetwork(network.Server)

		// init new irc handler
		handler := NewHandler(network, s.filterService, s.releaseService, definitions)

		// use network.Server + nick to use multiple indexers with different nick per network
		// this allows for multiple handlers to one network
		s.handlers[handlerKey{network.Server, network.NickServ.Account}] = handler
		s.lock.Unlock()

		log.Debug().Msgf("starting network: %+v", network.Name)

		s.stopWG.Add(1)

		go func() {
			if err := handler.Run(); err != nil {
				log.Error().Err(err).Msgf("failed to start handler for network %q", network.Name)
			}
		}()

		s.stopWG.Done()
	}
}

func (s *service) StopHandlers() {
	for _, handler := range s.handlers {
		log.Info().Msgf("stopping network: %+v", handler.network.Name)
		handler.Stop()
	}

	log.Info().Msg("stopped all irc handlers")
}

func (s *service) startNetwork(network domain.IrcNetwork) error {
	// look if we have the network in handlers already, if so start it
	if existingHandler, found := s.handlers[handlerKey{network.Server, network.NickServ.Account}]; found {
		log.Debug().Msgf("starting network: %+v", network.Name)

		if !existingHandler.client.Connected() {
			go func() {
				if err := existingHandler.Run(); err != nil {
					log.Error().Err(err).Msgf("failed to start existingHandler for network %q", existingHandler.network.Name)
				}
			}()
		}
	} else {
		// if not found in handlers, lets add it and run it

		s.lock.Lock()
		channels, err := s.repo.ListChannels(network.ID)
		if err != nil {
			log.Error().Err(err).Msgf("failed to list channels for network %q", network.Server)
		}
		network.Channels = channels

		// find indexer definitions for network and add
		definitions := s.indexerService.GetIndexersByIRCNetwork(network.Server)

		// init new irc handler
		handler := NewHandler(network, s.filterService, s.releaseService, definitions)

		s.handlers[handlerKey{network.Server, network.NickServ.Account}] = handler
		s.lock.Unlock()

		log.Debug().Msgf("starting network: %+v", network.Name)

		s.stopWG.Add(1)

		go func() {
			if err := handler.Run(); err != nil {
				log.Error().Err(err).Msgf("failed to start handler for network %q", network.Name)
			}
		}()

		s.stopWG.Done()
	}

	return nil
}

func (s *service) checkIfNetworkRestartNeeded(network *domain.IrcNetwork) error {
	// look if we have the network in handlers, if so restart it
	if existingHandler, found := s.handlers[handlerKey{network.Server, network.NickServ.Account}]; found {
		log.Debug().Msgf("irc: decide if irc network handler needs restart or updating: %+v", network.Server)

		// if server, tls, invite command, port : changed - restart
		// if nickserv account, nickserv password : changed - stay connected, and change those
		// if channels len : changes - join or leave
		if existingHandler.client.Connected() {
			handler := existingHandler.GetNetwork()
			restartNeeded := false

			if handler.Server != network.Server {
				restartNeeded = true
			} else if handler.Port != network.Port {
				restartNeeded = true
			} else if handler.TLS != network.TLS {
				restartNeeded = true
			} else if handler.InviteCommand != network.InviteCommand {
				restartNeeded = true
			}
			if restartNeeded {
				log.Info().Msgf("irc: restarting network: %+v", network.Server)

				// we need to reinitialize with new network config
				existingHandler.UpdateNetwork(network)

				// todo reset channelHealth?

				go func() {
					if err := existingHandler.Restart(); err != nil {
						log.Error().Stack().Err(err).Msgf("failed to restart network %q", existingHandler.network.Name)
					}
				}()

				// return now since the restart will read the network again
				return nil
			}

			if handler.NickServ.Account != network.NickServ.Account {
				log.Debug().Msg("changing nick")

				err := existingHandler.HandleNickChange(network.NickServ.Account)
				if err != nil {
					log.Error().Stack().Err(err).Msgf("failed to change nick %q", network.NickServ.Account)
				}
			} else if handler.NickServ.Password != network.NickServ.Password {
				log.Debug().Msg("nickserv: changing password")

				err := existingHandler.HandleNickServIdentify(network.NickServ.Account, network.NickServ.Password)
				if err != nil {
					log.Error().Stack().Err(err).Msgf("failed to identify with nickserv %q", network.NickServ.Account)
				}
			}

			// join or leave channels
			// loop over handler channels,
			var expectedChannels = make(map[string]struct{}, 0)
			var handlerChannels = make(map[string]struct{}, 0)
			var channelsToLeave = make([]string, 0)
			var channelsToJoin = make([]domain.IrcChannel, 0)

			// create map of expected channels
			for _, channel := range network.Channels {
				expectedChannels[channel.Name] = struct{}{}
			}

			// check current channels of handler against expected
			for _, handlerChan := range handler.Channels {
				handlerChannels[handlerChan.Name] = struct{}{}

				_, ok := expectedChannels[handlerChan.Name]
				if ok {
					// 	if handler channel matches network channel next
					continue
				}

				// if not expected, leave
				channelsToLeave = append(channelsToLeave, handlerChan.Name)
			}

			// check new channels against handler to see which to join
			for _, channel := range network.Channels {
				_, ok := handlerChannels[channel.Name]
				if ok {
					continue
				}

				// if expected channel not in handler channels, add to join
				// use channel struct for extra info
				channelsToJoin = append(channelsToJoin, channel)
			}

			// leave channels
			for _, leaveChannel := range channelsToLeave {
				log.Debug().Msgf("%v: part channel %v", network.Server, leaveChannel)
				err := existingHandler.HandlePartChannel(leaveChannel)
				if err != nil {
					log.Error().Stack().Err(err).Msgf("failed to leave channel: %q", leaveChannel)
				}
			}

			// join channels
			for _, joinChannel := range channelsToJoin {
				log.Debug().Msgf("%v: join new channel %v", network.Server, joinChannel)
				err := existingHandler.HandleJoinChannel(joinChannel.Name, joinChannel.Password)
				if err != nil {
					log.Error().Stack().Err(err).Msgf("failed to join channel: %q", joinChannel.Name)
				}
			}

			// update network for handler
			// TODO move all this restart logic inside handler to let it decide what to do
			existingHandler.SetNetwork(network)

			// find indexer definitions for network and add
			definitions := s.indexerService.GetIndexersByIRCNetwork(network.Server)

			existingHandler.InitIndexers(definitions)
		}
	} else {
		err := s.startNetwork(*network)
		if err != nil {
			log.Error().Stack().Err(err).Msgf("failed to start network: %q", network.Name)
		}
	}

	return nil
}

func (s *service) restartNetwork(network domain.IrcNetwork) error {
	// look if we have the network in handlers, if so restart it
	if existingHandler, found := s.handlers[handlerKey{network.Server, network.NickServ.Account}]; found {
		log.Info().Msgf("restarting network: %v", network.Name)

		if existingHandler.client.Connected() {
			go func() {
				if err := existingHandler.Restart(); err != nil {
					log.Error().Err(err).Msgf("failed to restart network %q", existingHandler.network.Name)
				}
			}()
		}
	}

	// TODO handle full restart

	return nil
}

func (s *service) StopNetwork(key handlerKey) error {
	if handler, found := s.handlers[key]; found {
		handler.Stop()
		log.Debug().Msgf("stopped network: %+v", key.server)
	}

	return nil
}

func (s *service) StopAndRemoveNetwork(key handlerKey) error {
	if handler, found := s.handlers[key]; found {
		handler.Stop()

		// remove from handlers
		delete(s.handlers, key)
		log.Debug().Msgf("stopped network: %+v", key)
	}

	return nil
}

func (s *service) StopNetworkIfRunning(key handlerKey) error {
	if handler, found := s.handlers[key]; found {
		handler.Stop()
		log.Debug().Msgf("stopped network: %+v", key.server)
	}

	return nil
}

func (s *service) GetNetworkByID(id int64) (*domain.IrcNetwork, error) {
	network, err := s.repo.GetNetworkByID(id)
	if err != nil {
		log.Error().Err(err).Msgf("failed to get network: %v", id)
		return nil, err
	}

	channels, err := s.repo.ListChannels(network.ID)
	if err != nil {
		log.Error().Err(err).Msgf("failed to list channels for network %q", network.Server)
		return nil, err
	}
	network.Channels = append(network.Channels, channels...)

	return network, nil
}

func (s *service) ListNetworks(ctx context.Context) ([]domain.IrcNetwork, error) {
	networks, err := s.repo.ListNetworks(ctx)
	if err != nil {
		log.Error().Err(err).Msgf("failed to list networks: %v", err)
		return nil, err
	}

	var ret []domain.IrcNetwork

	for _, n := range networks {
		channels, err := s.repo.ListChannels(n.ID)
		if err != nil {
			log.Error().Msgf("failed to list channels for network %q: %v", n.Server, err)
			return nil, err
		}
		n.Channels = append(n.Channels, channels...)

		ret = append(ret, n)
	}

	return ret, nil
}

func (s *service) GetNetworksWithHealth(ctx context.Context) ([]domain.IrcNetworkWithHealth, error) {
	networks, err := s.repo.ListNetworks(ctx)
	if err != nil {
		log.Error().Err(err).Msgf("failed to list networks: %v", err)
		return nil, err
	}

	var ret []domain.IrcNetworkWithHealth

	for _, n := range networks {
		netw := domain.IrcNetworkWithHealth{
			ID:            n.ID,
			Name:          n.Name,
			Enabled:       n.Enabled,
			Server:        n.Server,
			Port:          n.Port,
			TLS:           n.TLS,
			Pass:          n.Pass,
			InviteCommand: n.InviteCommand,
			NickServ:      n.NickServ,
			Connected:     false,
			Channels:      []domain.ChannelWithHealth{},
		}

		handler, ok := s.handlers[handlerKey{n.Server, n.NickServ.Account}]
		if ok {
			// only set connected and connected since if we have an active handler and connection
			if handler.client.Connected() {
				handler.m.RLock()
				netw.Connected = handler.connected
				netw.ConnectedSince = handler.connectedSince
				handler.m.RUnlock()
			}
		}

		channels, err := s.repo.ListChannels(n.ID)
		if err != nil {
			log.Error().Msgf("failed to list channels for network %q: %v", n.Server, err)
			return nil, err
		}

		// combine from repo and handler
		for _, channel := range channels {
			ch := domain.ChannelWithHealth{
				ID:       channel.ID,
				Enabled:  channel.Enabled,
				Name:     channel.Name,
				Password: channel.Password,
				Detached: channel.Detached,
				//Monitoring:      false,
				//MonitoringSince: time.Time{},
				//LastAnnounce:    time.Time{},
			}

			// only check if we have a handler
			if handler != nil {
				name := strings.ToLower(channel.Name)

				chan1, ok := handler.channelHealth[name]
				if ok {
					chan1.m.RLock()
					ch.Monitoring = chan1.monitoring
					ch.MonitoringSince = chan1.monitoringSince
					ch.LastAnnounce = chan1.lastAnnounce

					chan1.m.RUnlock()
				}
			}

			netw.Channels = append(netw.Channels, ch)
		}

		ret = append(ret, netw)
	}

	return ret, nil
}

func (s *service) DeleteNetwork(ctx context.Context, id int64) error {
	network, err := s.GetNetworkByID(id)
	if err != nil {
		return err
	}

	log.Debug().Msgf("delete network: %v", id)

	// Remove network and handler
	//if err = s.StopNetwork(network.Server); err != nil {
	if err = s.StopAndRemoveNetwork(handlerKey{network.Server, network.NickServ.Account}); err != nil {
		return err
	}

	if err = s.repo.DeleteNetwork(ctx, id); err != nil {
		return err
	}

	return nil
}

func (s *service) UpdateNetwork(ctx context.Context, network *domain.IrcNetwork) error {

	if network.Channels != nil {
		if err := s.repo.StoreNetworkChannels(ctx, network.ID, network.Channels); err != nil {
			return err
		}
	}

	if err := s.repo.UpdateNetwork(ctx, network); err != nil {
		return err
	}
	log.Debug().Msgf("irc.service: update network: %+v", network)

	// stop or start network
	// TODO get current state to see if enabled or not?
	if network.Enabled {
		// if server, tls, invite command, port : changed - restart
		// if nickserv account, nickserv password : changed - stay connected, and change those
		// if channels len : changes - join or leave
		err := s.checkIfNetworkRestartNeeded(network)
		if err != nil {
			log.Error().Stack().Err(err).Msgf("could not restart network: %+v", network.Name)
			return fmt.Errorf("could not restart network: %v", network.Name)
		}

	} else {
		// take into account multiple channels per network
		err := s.StopAndRemoveNetwork(handlerKey{network.Server, network.NickServ.Account})
		if err != nil {
			log.Error().Stack().Err(err).Msgf("could not stop network: %+v", network.Name)
			return fmt.Errorf("could not stop network: %v", network.Name)
		}
	}

	return nil
}

func (s *service) StoreNetwork(ctx context.Context, network *domain.IrcNetwork) error {
	existingNetwork, err := s.repo.CheckExistingNetwork(ctx, network)
	if err != nil {
		log.Error().Err(err).Msg("could not check for existing network")
		return err
	}

	if existingNetwork == nil {
		if err := s.repo.StoreNetwork(network); err != nil {
			return err
		}
		log.Debug().Msgf("store network: %+v", network)

		if network.Channels != nil {
			for _, channel := range network.Channels {
				if err := s.repo.StoreChannel(network.ID, &channel); err != nil {
					return err
				}
			}
		}

		return nil
	}

	// get channels for existing network
	existingChannels, err := s.repo.ListChannels(existingNetwork.ID)
	if err != nil {
		log.Error().Err(err).Msgf("failed to list channels for network %q", existingNetwork.Server)
	}
	existingNetwork.Channels = existingChannels

	if network.Channels != nil {
		for _, channel := range network.Channels {
			// add channels. Make sure it doesn't delete before
			if err := s.repo.StoreChannel(existingNetwork.ID, &channel); err != nil {
				return err
			}
		}

		// append channels to existing network
		existingNetwork.Channels = append(existingNetwork.Channels, network.Channels...)
	}

	if existingNetwork.Enabled {
		// if server, tls, invite command, port : changed - restart
		// if nickserv account, nickserv password : changed - stay connected, and change those
		// if channels len : changes - join or leave

		err := s.checkIfNetworkRestartNeeded(existingNetwork)
		if err != nil {
			log.Error().Err(err).Msgf("could not restart network: %+v", existingNetwork.Name)
			return fmt.Errorf("could not restart network: %v", existingNetwork.Name)
		}
	}

	return nil
}

func (s *service) StoreChannel(networkID int64, channel *domain.IrcChannel) error {
	if err := s.repo.StoreChannel(networkID, channel); err != nil {
		return err
	}

	return nil
}
