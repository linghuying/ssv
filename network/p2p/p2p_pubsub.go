package p2pv1

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/rand"
	"time"

	"github.com/bloxapp/ssv/protocol/v2/message"

	spectypes "github.com/bloxapp/ssv-spec/types"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/pkg/errors"
	"go.uber.org/zap"

	"github.com/bloxapp/ssv/logging/fields"
	"github.com/bloxapp/ssv/network"
	"github.com/bloxapp/ssv/network/commons"
	"github.com/bloxapp/ssv/network/records"
	p2pprotocol "github.com/bloxapp/ssv/protocol/v2/p2p"
	"github.com/bloxapp/ssv/protocol/v2/ssv/queue"
)

type validatorStatus int

const (
	validatorStatusInactive    validatorStatus = 0
	validatorStatusSubscribing validatorStatus = 1
	validatorStatusSubscribed  validatorStatus = 2
)

// UseMessageRouter registers a message router to handle incoming messages
func (n *p2pNetwork) UseMessageRouter(router network.MessageRouter) {
	n.msgRouter = router
}

// Peers registers a message router to handle incoming messages
func (n *p2pNetwork) Peers(pk spectypes.ValidatorPK) ([]peer.ID, error) {
	all := make([]peer.ID, 0)
	topics := commons.ValidatorTopicID(pk)
	for _, topic := range topics {
		peers, err := n.topicsCtrl.Peers(topic)
		if err != nil {
			return nil, err
		}
		all = append(all, peers...)
	}
	return all, nil
}

// Broadcast publishes the message to all peers in subnet
func (n *p2pNetwork) Broadcast(msg *spectypes.SSVMessage) error {
	if !n.isReady() {
		return p2pprotocol.ErrNetworkIsNotReady
	}

	if !n.operatorDataStore.OperatorIDReady() {
		return fmt.Errorf("operator ID is not ready")
	}

	encodedMsg, err := commons.EncodeNetworkMsg(msg)
	if err != nil {
		return errors.Wrap(err, "could not decode msg")
	}

	if n.cfg.Network.Beacon.EstimatedCurrentEpoch() > n.cfg.Network.PermissionlessActivationEpoch {
		signature, err := n.operatorSigner.Sign(encodedMsg)
		if err != nil {
			return err
		}

		encodedMsg = commons.EncodeSignedSSVMessage(encodedMsg, n.operatorDataStore.GetOperatorID(), signature)
	}

	vpk := msg.GetID().GetPubKey()
	topics := commons.ValidatorTopicID(vpk)

	for _, topic := range topics {
		if err := n.topicsCtrl.Broadcast(topic, encodedMsg, n.cfg.RequestTimeout); err != nil {
			n.interfaceLogger.Debug("could not broadcast msg", fields.PubKey(vpk), zap.Error(err))
			return errors.Wrap(err, "could not broadcast msg")
		}
	}
	return nil
}

func (n *p2pNetwork) SubscribeAll(logger *zap.Logger) error {
	if !n.isReady() {
		return p2pprotocol.ErrNetworkIsNotReady
	}
	n.subnets, _ = records.Subnets{}.FromString(records.AllSubnets)
	for subnet := 0; subnet < commons.Subnets(); subnet++ {
		err := n.topicsCtrl.Subscribe(logger, commons.SubnetTopicID(subnet))
		if err != nil {
			return err
		}
	}
	return nil
}

// SubscribeRandoms subscribes to random subnets. This method isn't thread-safe.
func (n *p2pNetwork) SubscribeRandoms(logger *zap.Logger, numSubnets int) error {
	if !n.isReady() {
		return p2pprotocol.ErrNetworkIsNotReady
	}
	if numSubnets > commons.Subnets() {
		numSubnets = commons.Subnets()
	}

	// Subscribe to random subnets.
	// #nosec G404
	randomSubnets := rand.New(rand.NewSource(time.Now().UnixNano())).Perm(commons.Subnets())
	randomSubnets = randomSubnets[:numSubnets]
	for _, subnet := range randomSubnets {
		err := n.topicsCtrl.Subscribe(logger, commons.SubnetTopicID(subnet))
		if err != nil {
			return fmt.Errorf("could not subscribe to subnet %d: %w", subnet, err)
		}
	}

	// Update the subnets slice.
	subnets := make([]byte, commons.Subnets())
	copy(subnets, n.subnets)
	for _, subnet := range randomSubnets {
		subnets[subnet] = byte(1)
	}
	n.subnets = subnets

	return nil
}

// Subscribe subscribes to validator subnet
func (n *p2pNetwork) Subscribe(pk spectypes.ValidatorPK) error {
	if !n.isReady() {
		return p2pprotocol.ErrNetworkIsNotReady
	}
	pkHex := hex.EncodeToString(pk)
	status, found := n.activeValidators.GetOrInsert(pkHex, validatorStatusSubscribing)
	if found && status != validatorStatusInactive {
		return nil
	}
	err := n.subscribe(n.interfaceLogger, pk)
	if err != nil {
		return err
	}
	n.activeValidators.Set(pkHex, validatorStatusSubscribed)
	return nil
}

// Unsubscribe unsubscribes from the validator subnet
func (n *p2pNetwork) Unsubscribe(logger *zap.Logger, pk spectypes.ValidatorPK) error {
	if !n.isReady() {
		return p2pprotocol.ErrNetworkIsNotReady
	}
	pkHex := hex.EncodeToString(pk)
	if status, _ := n.activeValidators.Get(pkHex); status != validatorStatusSubscribed {
		return nil
	}
	topics := commons.ValidatorTopicID(pk)
	for _, topic := range topics {
		if err := n.topicsCtrl.Unsubscribe(logger, topic, false); err != nil {
			return err
		}
	}
	n.activeValidators.Del(pkHex)
	return nil
}

// subscribe to validator topics, as defined in the fork
func (n *p2pNetwork) subscribe(logger *zap.Logger, pk spectypes.ValidatorPK) error {
	topics := commons.ValidatorTopicID(pk)
	for _, topic := range topics {
		if err := n.topicsCtrl.Subscribe(logger, topic); err != nil {
			// return errors.Wrap(err, "could not broadcast message")
			return err
		}
	}
	return nil
}

// handlePubsubMessages reads messages from the given channel and calls the router, note that this function blocks.
func (n *p2pNetwork) handlePubsubMessages(logger *zap.Logger) func(ctx context.Context, topic string, msg *pubsub.Message) error {
	return func(ctx context.Context, topic string, msg *pubsub.Message) error {
		if n.msgRouter == nil {
			logger.Debug("msg router is not configured")
			return nil
		}
		if msg == nil {
			return nil
		}

		var decodedMsg *queue.DecodedSSVMessage
		if msg.ValidatorData != nil {
			m, ok := msg.ValidatorData.(*queue.DecodedSSVMessage)
			if ok {
				decodedMsg = m
			}
		}
		if decodedMsg == nil {
			return errors.New("message was not decoded")
		}

		//p2pID := decodedMsg.GetID().String()

		//	logger.With(
		// 		zap.String("pubKey", hex.EncodeToString(ssvMsg.MsgID.GetPubKey())),
		// 		zap.String("role", ssvMsg.MsgID.GetRoleType().String()),
		// 	).Debug("handlePubsubMessages")

		metricsRouterIncoming.WithLabelValues(message.MsgTypeToString(decodedMsg.MsgType)).Inc()

		n.msgRouter.Route(ctx, decodedMsg)

		return nil
	}
}

// subscribeToSubnets subscribes to all the node's subnets
func (n *p2pNetwork) subscribeToSubnets(logger *zap.Logger) error {
	if len(n.subnets) == 0 {
		return nil
	}
	logger.Debug("subscribing to subnets", fields.Subnets(n.subnets))
	for i, val := range n.subnets {
		if val > 0 {
			subnet := fmt.Sprintf("%d", i)
			if err := n.topicsCtrl.Subscribe(logger, subnet); err != nil {
				logger.Warn("could not subscribe to subnet",
					zap.String("subnet", subnet), zap.Error(err))
				// TODO: handle error
			}
		}
	}
	return nil
}
