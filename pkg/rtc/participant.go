package rtc

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	lru "github.com/hashicorp/golang-lru"
	"github.com/livekit/livekit-server/pkg/sfu"
	"github.com/livekit/livekit-server/pkg/sfu/twcc"
	"github.com/livekit/protocol/logger"
	livekit "github.com/livekit/protocol/proto"
	"github.com/livekit/protocol/utils"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"github.com/pkg/errors"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/livekit-server/pkg/telemetry/prometheus"
	"github.com/livekit/livekit-server/version"
)

const (
	lossyDataChannel    = "_lossy"
	reliableDataChannel = "_reliable"
	sdBatchSize         = 20
)

type ParticipantParams struct {
	Identity        string
	Config          *WebRTCConfig
	Sink            routing.MessageSink
	AudioConfig     config.AudioConfig
	ProtocolVersion types.ProtocolVersion
	Telemetry       telemetry.TelemetryService
	ThrottleConfig  config.PLIThrottleConfig
	EnabledCodecs   []*livekit.Codec
	Hidden          bool
	Logger          logger.Logger
}

type ParticipantImpl struct {
	params      ParticipantParams
	id          string
	publisher   *PCTransport
	subscriber  *PCTransport
	isClosed    utils.AtomicFlag
	permission  *livekit.ParticipantPermission
	state       atomic.Value // livekit.ParticipantInfo_State
	rtcpCh      chan []rtcp.Packet
	pliThrottle *pliThrottle
	updateCache *lru.Cache

	// reliable and unreliable data channels
	reliableDC    *webrtc.DataChannel
	reliableDCSub *webrtc.DataChannel
	lossyDC       *webrtc.DataChannel
	lossyDCSub    *webrtc.DataChannel

	// when first connected
	connectedAt time.Time

	// JSON encoded metadata to pass to clients
	metadata string

	// hold reference for MediaTrack
	twcc *twcc.Responder

	// tracks the current participant is subscribed to, map of sid => DownTrack
	subscribedTracks map[string]types.SubscribedTrack
	// publishedTracks that participant is publishing
	publishedTracks map[string]types.PublishedTrack
	// client intended to publish, yet to be reconciled
	pendingTracks map[string]*livekit.TrackInfo
	// keep track of other publishers identities that we are subscribed to
	subscribedTo sync.Map // string => struct{}

	lock       sync.RWMutex
	once       sync.Once
	updateLock sync.Mutex

	// callbacks & handlers
	onTrackPublished func(types.Participant, types.PublishedTrack)
	onTrackUpdated   func(types.Participant, types.PublishedTrack)
	onStateChange    func(p types.Participant, oldState livekit.ParticipantInfo_State)
	onMetadataUpdate func(types.Participant)
	onDataPacket     func(types.Participant, *livekit.DataPacket)
	onClose          func(types.Participant)
}

func NewParticipant(params ParticipantParams) (*ParticipantImpl, error) {
	// TODO: check to ensure params are valid, id and identity can't be empty

	p := &ParticipantImpl{
		params:           params,
		id:               utils.NewGuid(utils.ParticipantPrefix),
		rtcpCh:           make(chan []rtcp.Packet, 50),
		pliThrottle:      newPLIThrottle(params.ThrottleConfig),
		subscribedTracks: make(map[string]types.SubscribedTrack),
		publishedTracks:  make(map[string]types.PublishedTrack, 0),
		pendingTracks:    make(map[string]*livekit.TrackInfo),
		connectedAt:      time.Now(),
	}
	p.state.Store(livekit.ParticipantInfo_JOINING)

	var err error
	// keep last participants and when updates were sent
	if p.updateCache, err = lru.New(32); err != nil {
		return nil, err
	}
	p.publisher, err = NewPCTransport(TransportParams{
		ParticipantID:       p.id,
		ParticipantIdentity: p.params.Identity,
		Target:              livekit.SignalTarget_PUBLISHER,
		Config:              params.Config,
		Telemetry:           p.params.Telemetry,
		EnabledCodecs:       p.params.EnabledCodecs,
		Logger:              params.Logger,
	})
	if err != nil {
		return nil, err
	}
	p.subscriber, err = NewPCTransport(TransportParams{
		ParticipantID:       p.id,
		ParticipantIdentity: p.params.Identity,
		Target:              livekit.SignalTarget_SUBSCRIBER,
		Config:              params.Config,
		Telemetry:           p.params.Telemetry,
		EnabledCodecs:       p.params.EnabledCodecs,
		Logger:              params.Logger,
	})
	if err != nil {
		return nil, err
	}

	p.publisher.pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil || p.State() == livekit.ParticipantInfo_DISCONNECTED {
			return
		}
		p.sendIceCandidate(c, livekit.SignalTarget_PUBLISHER)
	})
	p.subscriber.pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil || p.State() == livekit.ParticipantInfo_DISCONNECTED {
			return
		}
		p.sendIceCandidate(c, livekit.SignalTarget_SUBSCRIBER)
	})

	primaryPC := p.publisher.pc

	if p.SubscriberAsPrimary() {
		primaryPC = p.subscriber.pc
		ordered := true
		// also create data channels for subs
		p.reliableDCSub, err = primaryPC.CreateDataChannel(reliableDataChannel, &webrtc.DataChannelInit{
			Ordered: &ordered,
		})
		if err != nil {
			return nil, err
		}
		retransmits := uint16(0)
		p.lossyDCSub, err = primaryPC.CreateDataChannel(lossyDataChannel, &webrtc.DataChannelInit{
			Ordered:        &ordered,
			MaxRetransmits: &retransmits,
		})
		if err != nil {
			return nil, err
		}
	}
	primaryPC.OnICEConnectionStateChange(p.handlePrimaryICEStateChange)
	p.publisher.pc.OnTrack(p.onMediaTrack)
	p.publisher.pc.OnDataChannel(p.onDataChannel)

	p.subscriber.OnOffer(p.onOffer)

	p.subscriber.OnStreamedTracksChange(p.onStreamedTracksChange)

	return p, nil
}

func (p *ParticipantImpl) ID() string {
	return p.id
}

func (p *ParticipantImpl) Identity() string {
	return p.params.Identity
}

func (p *ParticipantImpl) State() livekit.ParticipantInfo_State {
	return p.state.Load().(livekit.ParticipantInfo_State)
}

func (p *ParticipantImpl) ProtocolVersion() types.ProtocolVersion {
	return p.params.ProtocolVersion
}

func (p *ParticipantImpl) IsReady() bool {
	state := p.State()
	return state == livekit.ParticipantInfo_JOINED || state == livekit.ParticipantInfo_ACTIVE
}

func (p *ParticipantImpl) ConnectedAt() time.Time {
	return p.connectedAt
}

// SetMetadata attaches metadata to the participant
func (p *ParticipantImpl) SetMetadata(metadata string) {
	p.metadata = metadata

	if p.onMetadataUpdate != nil {
		p.onMetadataUpdate(p)
	}
}

func (p *ParticipantImpl) SetPermission(permission *livekit.ParticipantPermission) {
	p.permission = permission
}

func (p *ParticipantImpl) RTCPChan() chan []rtcp.Packet {
	return p.rtcpCh
}

func (p *ParticipantImpl) ToProto() *livekit.ParticipantInfo {
	info := &livekit.ParticipantInfo{
		Sid:      p.id,
		Identity: p.params.Identity,
		Metadata: p.metadata,
		State:    p.State(),
		JoinedAt: p.ConnectedAt().Unix(),
		Hidden:   p.Hidden(),
	}

	p.lock.RLock()
	for _, t := range p.publishedTracks {
		info.Tracks = append(info.Tracks, t.ToProto())
	}
	p.lock.RUnlock()
	return info
}

func (p *ParticipantImpl) GetResponseSink() routing.MessageSink {
	return p.params.Sink
}

func (p *ParticipantImpl) SetResponseSink(sink routing.MessageSink) {
	p.params.Sink = sink
}

func (p *ParticipantImpl) SubscriberMediaEngine() *webrtc.MediaEngine {
	return p.subscriber.me
}

// callbacks for clients

func (p *ParticipantImpl) OnTrackPublished(callback func(types.Participant, types.PublishedTrack)) {
	p.onTrackPublished = callback
}

func (p *ParticipantImpl) OnStateChange(callback func(p types.Participant, oldState livekit.ParticipantInfo_State)) {
	p.onStateChange = callback
}

func (p *ParticipantImpl) OnTrackUpdated(callback func(types.Participant, types.PublishedTrack)) {
	p.onTrackUpdated = callback
}

func (p *ParticipantImpl) OnMetadataUpdate(callback func(types.Participant)) {
	p.onMetadataUpdate = callback
}

func (p *ParticipantImpl) OnDataPacket(callback func(types.Participant, *livekit.DataPacket)) {
	p.onDataPacket = callback
}

func (p *ParticipantImpl) OnClose(callback func(types.Participant)) {
	p.onClose = callback
}

// HandleOffer an offer from remote participant, used when clients make the initial connection
func (p *ParticipantImpl) HandleOffer(sdp webrtc.SessionDescription) (answer webrtc.SessionDescription, err error) {
	p.params.Logger.Debugw("answering pub offer", "state", p.State().String(),
		"participant", p.Identity(), "pID", p.ID(),
		//"sdp", sdp.SDP,
	)

	if err = p.publisher.SetRemoteDescription(sdp); err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("answer", "error", "remote_description").Add(1)
		return
	}

	p.configureReceiverDTX()

	answer, err = p.publisher.pc.CreateAnswer(nil)
	if err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("answer", "error", "create").Add(1)
		err = errors.Wrap(err, "could not create answer")
		return
	}

	if err = p.publisher.pc.SetLocalDescription(answer); err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("answer", "error", "local_description").Add(1)
		err = errors.Wrap(err, "could not set local description")
		return
	}

	p.params.Logger.Debugw("sending answer to client",
		"participant", p.Identity(), "pID", p.ID(),
		//"answer sdp", answer.SDP,
	)
	err = p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Answer{
			Answer: ToProtoSessionDescription(answer),
		},
	})
	if err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("answer", "error", "write_message").Add(1)
		return
	}

	if p.State() == livekit.ParticipantInfo_JOINING {
		p.updateState(livekit.ParticipantInfo_JOINED)
	}
	prometheus.ServiceOperationCounter.WithLabelValues("answer", "success", "").Add(1)

	return
}

// AddTrack is called when client intends to publish track.
// records track details and lets client know it's ok to proceed
func (p *ParticipantImpl) AddTrack(req *livekit.AddTrackRequest) {
	p.lock.Lock()
	defer p.lock.Unlock()

	// if track is already published, reject
	if p.pendingTracks[req.Cid] != nil {
		return
	}

	if p.getPublishedTrackBySignalCid(req.Cid) != nil || p.getPublishedTrackBySdpCid(req.Cid) != nil {
		return
	}

	if !p.CanPublish() {
		p.params.Logger.Warnw("no permission to publish track", nil,
			"participant", p.Identity(), "pID", p.ID())
		return
	}

	ti := &livekit.TrackInfo{
		Type:       req.Type,
		Name:       req.Name,
		Sid:        utils.NewGuid(utils.TrackPrefix),
		Width:      req.Width,
		Height:     req.Height,
		Muted:      req.Muted,
		DisableDtx: req.DisableDtx,
		Source:     req.Source,
	}
	p.pendingTracks[req.Cid] = ti

	_ = p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_TrackPublished{
			TrackPublished: &livekit.TrackPublishedResponse{
				Cid:   req.Cid,
				Track: ti,
			},
		},
	})
}

// HandleAnswer handles a client answer response, with subscriber PC, server initiates the
// offer and client answers
func (p *ParticipantImpl) HandleAnswer(sdp webrtc.SessionDescription) error {
	if sdp.Type != webrtc.SDPTypeAnswer {
		return ErrUnexpectedOffer
	}
	p.params.Logger.Debugw("setting subPC answer",
		"participant", p.Identity(), "pID", p.ID(),
		//"sdp", sdp.SDP,
	)

	if err := p.subscriber.SetRemoteDescription(sdp); err != nil {
		return errors.Wrap(err, "could not set remote description")
	}

	return nil
}

// AddICECandidate adds candidates for remote peer
func (p *ParticipantImpl) AddICECandidate(candidate webrtc.ICECandidateInit, target livekit.SignalTarget) error {
	var err error
	if target == livekit.SignalTarget_PUBLISHER {
		err = p.publisher.AddICECandidate(candidate)
	} else {
		err = p.subscriber.AddICECandidate(candidate)
	}
	return err
}

func (p *ParticipantImpl) Start() {
	p.once.Do(func() {
		go p.rtcpSendWorker()
		go p.downTracksRTCPWorker()
	})
}

func (p *ParticipantImpl) Close() error {
	if !p.isClosed.TrySet(true) {
		// already closed
		return nil
	}

	// send leave message
	_ = p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Leave{
			Leave: &livekit.LeaveRequest{},
		},
	})

	// remove all downtracks
	p.lock.Lock()
	for _, t := range p.publishedTracks {
		// skip updates
		t.RemoveAllSubscribers()
	}

	var downtracksToClose []*sfu.DownTrack
	for _, st := range p.subscribedTracks {
		downtracksToClose = append(downtracksToClose, st.DownTrack())
	}
	p.lock.Unlock()

	for _, dt := range downtracksToClose {
		dt.Close()
	}

	p.updateState(livekit.ParticipantInfo_DISCONNECTED)

	// ensure this is synchronized
	p.lock.RLock()
	p.params.Sink.Close()
	onClose := p.onClose
	p.lock.RUnlock()
	if onClose != nil {
		onClose(p)
	}
	p.publisher.Close()
	p.subscriber.Close()
	close(p.rtcpCh)
	return nil
}

func (p *ParticipantImpl) Negotiate() {
	p.subscriber.Negotiate()
}

// ICERestart restarts subscriber ICE connections
func (p *ParticipantImpl) ICERestart() error {
	if p.subscriber.pc.RemoteDescription() == nil {
		// not connected, skip
		return nil
	}
	return p.subscriber.CreateAndSendOffer(&webrtc.OfferOptions{
		ICERestart: true,
	})
}

// AddSubscriber subscribes op to all publishedTracks
func (p *ParticipantImpl) AddSubscriber(op types.Participant) (int, error) {
	tracks := p.GetPublishedTracks()

	if len(tracks) == 0 {
		return 0, nil
	}

	p.params.Logger.Debugw("subscribing new participant to tracks",
		"participants", []string{p.Identity(), op.Identity()},
		"pIDs", []string{p.ID(), op.ID()},
		"numTracks", len(tracks))

	n := 0
	for _, track := range tracks {
		if err := track.AddSubscriber(op); err != nil {
			return n, err
		}
		n += 1
	}
	return n, nil
}

func (p *ParticipantImpl) RemoveSubscriber(participantId string) {
	p.lock.RLock()
	defer p.lock.RUnlock()

	for _, track := range p.publishedTracks {
		track.RemoveSubscriber(participantId)
	}
}

// signal connection methods

func (p *ParticipantImpl) SendJoinResponse(
	roomInfo *livekit.Room,
	otherParticipants []*livekit.ParticipantInfo,
	iceServers []*livekit.ICEServer,
) error {
	// send Join response
	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Join{
			Join: &livekit.JoinResponse{
				Room:              roomInfo,
				Participant:       p.ToProto(),
				OtherParticipants: otherParticipants,
				ServerVersion:     version.Version,
				IceServers:        iceServers,
				// indicates both server and client support subscriber as primary
				SubscriberPrimary: p.SubscriberAsPrimary(),
			},
		},
	})
}

func (p *ParticipantImpl) SendParticipantUpdate(participantsToUpdate []*livekit.ParticipantInfo, updatedAt time.Time) error {
	if len(participantsToUpdate) == 1 {
		p.updateLock.Lock()
		defer p.updateLock.Unlock()
		pi := participantsToUpdate[0]
		if val, ok := p.updateCache.Get(pi.Sid); ok {
			if lastUpdatedAt, ok := val.(time.Time); ok {
				// this is a message delivered out of order, a more recent version of the message had already been
				// sent.
				if lastUpdatedAt.After(updatedAt) {
					return nil
				}
			}
		}
		p.updateCache.Add(pi.Sid, updatedAt)
	}
	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Update{
			Update: &livekit.ParticipantUpdate{
				Participants: participantsToUpdate,
			},
		},
	})
}

// SendSpeakerUpdate notifies participant changes to speakers. only send members that have changed since last update
func (p *ParticipantImpl) SendSpeakerUpdate(speakers []*livekit.SpeakerInfo) error {
	if !p.IsReady() {
		return nil
	}

	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_SpeakersChanged{
			SpeakersChanged: &livekit.SpeakersChanged{
				Speakers: speakers,
			},
		},
	})
}

func (p *ParticipantImpl) SendDataPacket(dp *livekit.DataPacket) error {
	if p.State() != livekit.ParticipantInfo_ACTIVE {
		return ErrDataChannelUnavailable
	}

	data, err := proto.Marshal(dp)
	if err != nil {
		return err
	}

	var dc *webrtc.DataChannel
	if dp.Kind == livekit.DataPacket_RELIABLE {
		if p.SubscriberAsPrimary() {
			dc = p.reliableDCSub
		} else {
			dc = p.reliableDC
		}
	} else {
		if p.SubscriberAsPrimary() {
			dc = p.lossyDCSub
		} else {
			dc = p.lossyDC
		}
	}

	if dc == nil {
		return ErrDataChannelUnavailable
	}
	return dc.Send(data)
}

func (p *ParticipantImpl) SendRoomUpdate(room *livekit.Room) error {
	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_RoomUpdate{
			RoomUpdate: &livekit.RoomUpdate{
				Room: room,
			},
		},
	})
}

func (p *ParticipantImpl) SendConnectionQualityUpdate(update *livekit.ConnectionQualityUpdate) error {
	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_ConnectionQuality{
			ConnectionQuality: update,
		},
	})
}

func (p *ParticipantImpl) SetTrackMuted(trackId string, muted bool, fromAdmin bool) {
	isPending := false
	p.lock.RLock()
	for _, ti := range p.pendingTracks {
		if ti.Sid == trackId {
			ti.Muted = muted
			isPending = true
		}
	}
	track := p.publishedTracks[trackId]
	p.lock.RUnlock()

	if track == nil {
		if !isPending {
			p.params.Logger.Warnw("could not locate track", nil, "track", trackId)
		}
		return
	}
	currentMuted := track.IsMuted()
	track.SetMuted(muted)

	// when request is coming from admin, send message to current participant
	if fromAdmin {
		_ = p.writeMessage(&livekit.SignalResponse{
			Message: &livekit.SignalResponse_Mute{
				Mute: &livekit.MuteTrackRequest{
					Sid:   trackId,
					Muted: muted,
				},
			},
		})
	}

	if currentMuted != track.IsMuted() && p.onTrackUpdated != nil {
		p.params.Logger.Debugw("mute status changed",
			"participant", p.Identity(),
			"pID", p.ID(),
			"track", trackId,
			"muted", track.IsMuted())
		p.onTrackUpdated(p, track)
	}
}

func (p *ParticipantImpl) GetAudioLevel() (level uint8, active bool) {
	p.lock.RLock()
	defer p.lock.RUnlock()
	level = silentAudioLevel
	for _, pt := range p.publishedTracks {
		if mt, ok := pt.(*MediaTrack); ok {
			if mt.audioLevel == nil {
				continue
			}
			tl, ta := mt.audioLevel.GetLevel()
			if ta {
				active = true
				if tl < level {
					level = tl
				}
			}
		}
	}
	return
}

func (p *ParticipantImpl) GetConnectionQuality() livekit.ConnectionQuality {
	// avg loss across all tracks, weigh published the same as subscribed
	var pubLoss, subLoss uint32
	var reducedQualityPub bool
	var reducedQualitySub bool
	p.lock.RLock()
	defer p.lock.RUnlock()
	for _, pubTrack := range p.publishedTracks {
		if pubTrack.IsMuted() {
			continue
		}
		pubLoss += pubTrack.PublishLossPercentage()
		publishing, registered := pubTrack.NumUpTracks()
		if registered > 0 && publishing != registered {
			reducedQualityPub = true
		}
	}
	numTracks := uint32(len(p.publishedTracks))
	if numTracks > 0 {
		pubLoss /= numTracks
	}

	for _, subTrack := range p.subscribedTracks {
		if subTrack.IsMuted() {
			continue
		}
		if subTrack.DownTrack().GetForwardingStatus() != sfu.ForwardingStatusOptimal {
			reducedQualitySub = true
		}
		subLoss += subTrack.SubscribeLossPercentage()
	}
	numTracks = uint32(len(p.subscribedTracks))
	if numTracks > 0 {
		subLoss /= numTracks
	}

	avgLoss := (pubLoss + subLoss) / 2
	if avgLoss >= 4 {
		return livekit.ConnectionQuality_POOR
	} else if avgLoss <= 2 && !reducedQualityPub && !reducedQualitySub {
		return livekit.ConnectionQuality_EXCELLENT
	}

	return livekit.ConnectionQuality_GOOD
}

func (p *ParticipantImpl) IsSubscribedTo(identity string) bool {
	_, ok := p.subscribedTo.Load(identity)
	return ok
}

func (p *ParticipantImpl) GetSubscribedParticipants() []string {
	var identities []string
	p.subscribedTo.Range(func(key, _ interface{}) bool {
		if identity, ok := key.(string); ok {
			identities = append(identities, identity)
		}
		return true
	})
	return identities
}

func (p *ParticipantImpl) CanPublish() bool {
	return p.permission == nil || p.permission.CanPublish
}

func (p *ParticipantImpl) CanSubscribe() bool {
	return p.permission == nil || p.permission.CanSubscribe
}

func (p *ParticipantImpl) CanPublishData() bool {
	return p.permission == nil || p.permission.CanPublishData
}

func (p *ParticipantImpl) Hidden() bool {
	return p.params.Hidden
}

func (p *ParticipantImpl) SubscriberAsPrimary() bool {
	return p.ProtocolVersion().SubscriberAsPrimary() && p.CanSubscribe()
}

func (p *ParticipantImpl) SubscriberPC() *webrtc.PeerConnection {
	return p.subscriber.pc
}

func (p *ParticipantImpl) GetPublishedTrack(sid string) types.PublishedTrack {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.publishedTracks[sid]
}

func (p *ParticipantImpl) GetPublishedTracks() []types.PublishedTrack {
	p.lock.RLock()
	defer p.lock.RUnlock()
	tracks := make([]types.PublishedTrack, 0, len(p.publishedTracks))
	for _, t := range p.publishedTracks {
		tracks = append(tracks, t)
	}
	return tracks
}

func (p *ParticipantImpl) GetSubscribedTrack(sid string) types.SubscribedTrack {
	p.lock.RLock()
	defer p.lock.RUnlock()
	return p.subscribedTracks[sid]
}

func (p *ParticipantImpl) GetSubscribedTracks() []types.SubscribedTrack {
	p.lock.RLock()
	defer p.lock.RUnlock()
	subscribed := make([]types.SubscribedTrack, 0, len(p.subscribedTracks))
	for _, st := range p.subscribedTracks {
		subscribed = append(subscribed, st)
	}
	return subscribed
}

// AddSubscribedTrack adds a track to the participant's subscribed list
func (p *ParticipantImpl) AddSubscribedTrack(subTrack types.SubscribedTrack) {
	p.params.Logger.Debugw("added subscribedTrack", "publisher", subTrack.PublisherIdentity(),
		"participant", p.Identity(), "track", subTrack.ID())
	p.lock.Lock()
	p.subscribedTracks[subTrack.ID()] = subTrack
	p.lock.Unlock()

	p.subscriber.AddTrack(subTrack)
	p.subscribedTo.Store(subTrack.PublisherIdentity(), struct{}{})
}

// RemoveSubscribedTrack removes a track to the participant's subscribed list
func (p *ParticipantImpl) RemoveSubscribedTrack(subTrack types.SubscribedTrack) {
	p.params.Logger.Debugw("removed subscribedTrack", "publisher", subTrack.PublisherIdentity(),
		"participant", p.Identity(), "track", subTrack.ID(), "kind", subTrack.DownTrack().Kind())

	p.subscriber.RemoveTrack(subTrack)

	p.lock.Lock()
	delete(p.subscribedTracks, subTrack.ID())
	// remove from subscribed map
	numRemaining := 0
	for _, st := range p.subscribedTracks {
		if st.PublisherIdentity() == subTrack.PublisherIdentity() {
			numRemaining++
		}
	}
	p.lock.Unlock()
	if numRemaining == 0 {
		p.subscribedTo.Delete(subTrack.PublisherIdentity())
	}
}

func (p *ParticipantImpl) sendIceCandidate(c *webrtc.ICECandidate, target livekit.SignalTarget) {
	ci := c.ToJSON()

	// write candidate
	p.params.Logger.Debugw("sending ice candidates",
		"participant", p.Identity(),
		"pID", p.ID(),
		"candidate", c.String())
	trickle := ToProtoTrickle(ci)
	trickle.Target = target
	_ = p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Trickle{
			Trickle: trickle,
		},
	})
}

func (p *ParticipantImpl) updateState(state livekit.ParticipantInfo_State) {
	oldState := p.State()
	if state == oldState {
		return
	}
	p.state.Store(state)
	p.params.Logger.Debugw("updating participant state", "state", state.String(), "participant", p.Identity(), "pID", p.ID())
	p.lock.RLock()
	onStateChange := p.onStateChange
	p.lock.RUnlock()
	if onStateChange != nil {
		go func() {
			defer Recover()
			onStateChange(p, oldState)
		}()
	}
}

func (p *ParticipantImpl) writeMessage(msg *livekit.SignalResponse) error {
	if p.State() == livekit.ParticipantInfo_DISCONNECTED {
		return nil
	}
	sink := p.params.Sink
	if sink == nil {
		return nil
	}
	err := sink.WriteMessage(msg)
	if err != nil {
		p.params.Logger.Warnw("could not send message to participant", err,
			"pID", p.ID(),
			"participant", p.Identity(),
			"message", fmt.Sprintf("%T", msg.Message))
		return err
	}
	return nil
}

// when the server has an offer for participant
func (p *ParticipantImpl) onOffer(offer webrtc.SessionDescription) {
	if p.State() == livekit.ParticipantInfo_DISCONNECTED {
		p.params.Logger.Debugw("skipping server offer", "participant", p.Identity(), "pID", p.ID())
		// skip when disconnected
		return
	}

	p.params.Logger.Debugw("sending server offer to participant",
		"participant", p.Identity(), "pID", p.ID(),
		//"sdp", offer.SDP,
	)

	err := p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Offer{
			Offer: ToProtoSessionDescription(offer),
		},
	})
	if err != nil {
		prometheus.ServiceOperationCounter.WithLabelValues("offer", "error", "write_message").Add(1)
	} else {
		prometheus.ServiceOperationCounter.WithLabelValues("offer", "success", "").Add(1)
	}
}

// when a new remoteTrack is created, creates a Track and adds it to room
func (p *ParticipantImpl) onMediaTrack(track *webrtc.TrackRemote, rtpReceiver *webrtc.RTPReceiver) {
	if p.State() == livekit.ParticipantInfo_DISCONNECTED {
		return
	}

	p.params.Logger.Debugw("mediaTrack added",
		"kind", track.Kind().String(),
		"participant", p.Identity(),
		"pID", p.ID(),
		"track", track.ID(),
		"rid", track.RID(),
		"SSRC", track.SSRC())

	if !p.CanPublish() {
		p.params.Logger.Warnw("no permission to publish mediaTrack", nil,
			"participant", p.Identity(), "pID", p.ID())
		return
	}

	var newTrack bool

	// use existing mediatrack to handle simulcast
	p.lock.Lock()
	mt, ok := p.getPublishedTrackBySdpCid(track.ID()).(*MediaTrack)
	if !ok {
		signalCid, ti := p.getPendingTrack(track.ID(), ToProtoTrackKind(track.Kind()))
		if ti == nil {
			p.lock.Unlock()
			return
		}

		mt = NewMediaTrack(track, MediaTrackParams{
			TrackInfo:           ti,
			SignalCid:           signalCid,
			SdpCid:              track.ID(),
			ParticipantID:       p.id,
			ParticipantIdentity: p.Identity(),
			RTCPChan:            p.rtcpCh,
			BufferFactory:       p.params.Config.BufferFactory,
			ReceiverConfig:      p.params.Config.Receiver,
			AudioConfig:         p.params.AudioConfig,
			Telemetry:           p.params.Telemetry,
			Logger:              p.params.Logger,
		})

		// add to published and clean up pending
		p.publishedTracks[mt.ID()] = mt
		delete(p.pendingTracks, signalCid)

		newTrack = true
	}

	ssrc := uint32(track.SSRC())
	p.pliThrottle.addTrack(ssrc, track.RID())
	if p.twcc == nil {
		p.twcc = twcc.NewTransportWideCCResponder(ssrc)
		p.twcc.OnFeedback(func(pkt rtcp.RawPacket) {
			_ = p.publisher.pc.WriteRTCP([]rtcp.Packet{&pkt})
		})
	}
	p.lock.Unlock()

	mt.AddReceiver(rtpReceiver, track, p.twcc)

	if newTrack {
		p.handleTrackPublished(mt)
	}
}

func (p *ParticipantImpl) onDataChannel(dc *webrtc.DataChannel) {
	if p.State() == livekit.ParticipantInfo_DISCONNECTED {
		return
	}
	switch dc.Label() {
	case reliableDataChannel:
		p.reliableDC = dc
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			p.handleDataMessage(livekit.DataPacket_RELIABLE, msg.Data)
		})
	case lossyDataChannel:
		p.lossyDC = dc
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			p.handleDataMessage(livekit.DataPacket_LOSSY, msg.Data)
		})
	default:
		p.params.Logger.Warnw("unsupported datachannel added", nil, "participant", p.Identity(), "pID", p.ID(), "label", dc.Label())
	}
}

// should be called with lock held
func (p *ParticipantImpl) getPublishedTrackBySignalCid(clientId string) types.PublishedTrack {
	for _, publishedTrack := range p.publishedTracks {
		if publishedTrack.SignalCid() == clientId {
			return publishedTrack
		}
	}

	return nil
}

// should be called with lock held
func (p *ParticipantImpl) getPublishedTrackBySdpCid(clientId string) types.PublishedTrack {
	for _, publishedTrack := range p.publishedTracks {
		if publishedTrack.SdpCid() == clientId {
			return publishedTrack
		}
	}

	return nil
}

// should be called with lock held
func (p *ParticipantImpl) getPendingTrack(clientId string, kind livekit.TrackType) (string, *livekit.TrackInfo) {
	signalCid := clientId
	ti := p.pendingTracks[clientId]

	// then find the first one that matches type. with MediaStreamTrack, it's possible for the client id to
	// change after being added to SubscriberPC
	if ti == nil {
		for cid, info := range p.pendingTracks {
			if info.Type == kind {
				ti = info
				signalCid = cid
				break
			}
		}
	}

	// if still not found, we are done
	if ti == nil {
		p.params.Logger.Errorw("track info not published prior to track", nil, "clientId", clientId)
	}
	return signalCid, ti
}

func (p *ParticipantImpl) handleDataMessage(kind livekit.DataPacket_Kind, data []byte) {
	dp := livekit.DataPacket{}
	if err := proto.Unmarshal(data, &dp); err != nil {
		p.params.Logger.Warnw("could not parse data packet", err)
		return
	}

	// trust the channel that it came in as the source of truth
	dp.Kind = kind

	// only forward on user payloads
	switch payload := dp.Value.(type) {
	case *livekit.DataPacket_User:
		if p.onDataPacket != nil {
			payload.User.ParticipantSid = p.id
			p.onDataPacket(p, &dp)
		}
	default:
		p.params.Logger.Warnw("received unsupported data packet", nil, "payload", payload)
	}
}

func (p *ParticipantImpl) handleTrackPublished(track types.PublishedTrack) {
	p.lock.Lock()
	if _, ok := p.publishedTracks[track.ID()]; !ok {
		p.publishedTracks[track.ID()] = track
	}
	p.lock.Unlock()

	track.Start()

	track.AddOnClose(func() {
		// cleanup
		p.lock.Lock()
		delete(p.publishedTracks, track.ID())
		p.lock.Unlock()
		// only send this when client is in a ready state
		if p.IsReady() && p.onTrackUpdated != nil {
			p.onTrackUpdated(p, track)
		}
	})

	if p.onTrackPublished != nil {
		p.onTrackPublished(p, track)
	}
}

func (p *ParticipantImpl) handlePrimaryICEStateChange(state webrtc.ICEConnectionState) {
	// p.params.Logger.Debugw("ICE connection state changed", "state", state.String(),
	//	"participant", p.identity, "pID", p.ID())
	if state == webrtc.ICEConnectionStateConnected {
		prometheus.ServiceOperationCounter.WithLabelValues("ice_connection", "success", "").Add(1)
		p.updateState(livekit.ParticipantInfo_ACTIVE)
	} else if state == webrtc.ICEConnectionStateFailed {
		// only close when failed, to allow clients opportunity to reconnect
		go func() {
			_ = p.Close()
		}()
	}
}

// downTracksRTCPWorker sends SenderReports periodically when the participant is subscribed to
// other publishedTracks in the room.
func (p *ParticipantImpl) downTracksRTCPWorker() {
	defer Recover()
	for {
		time.Sleep(5 * time.Second)

		if p.State() == livekit.ParticipantInfo_DISCONNECTED {
			return
		}
		if p.subscriber.pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
			continue
		}

		var srs []rtcp.Packet
		var sd []rtcp.SourceDescriptionChunk
		p.lock.RLock()
		for _, subTrack := range p.subscribedTracks {
			sr := subTrack.DownTrack().CreateSenderReport()
			chunks := subTrack.DownTrack().CreateSourceDescriptionChunks()
			if sr == nil || chunks == nil {
				continue
			}
			srs = append(srs, sr)
			sd = append(sd, chunks...)
		}
		p.lock.RUnlock()

		// now send in batches of sdBatchSize
		var batch []rtcp.SourceDescriptionChunk
		var pkts []rtcp.Packet
		batchSize := 0
		for len(sd) > 0 || len(srs) > 0 {
			numSRs := len(srs)
			if numSRs > 0 {
				if numSRs > sdBatchSize {
					numSRs = sdBatchSize
				}
				pkts = append(pkts, srs[:numSRs]...)
				srs = srs[numSRs:]
			}

			size := len(sd)
			spaceRemain := sdBatchSize - batchSize
			if spaceRemain > 0 && size > 0 {
				if size > spaceRemain {
					size = spaceRemain
				}
				batch = sd[:size]
				sd = sd[size:]
				pkts = append(pkts, &rtcp.SourceDescription{Chunks: batch})
				if err := p.subscriber.pc.WriteRTCP(pkts); err != nil {
					if err == io.EOF || err == io.ErrClosedPipe {
						return
					}
					logger.Errorw("could not send downtrack reports", err,
						"participant", p.Identity(), "pID", p.ID())
				}
			}

			pkts = pkts[:0]
			batchSize = 0
		}
	}
}

func (p *ParticipantImpl) rtcpSendWorker() {
	defer Recover()

	// read from rtcpChan
	for pkts := range p.rtcpCh {
		if pkts == nil {
			return
		}

		fwdPkts := make([]rtcp.Packet, 0, len(pkts))
		for _, pkt := range pkts {
			switch pkt.(type) {
			case *rtcp.PictureLossIndication:
				mediaSSRC := pkt.(*rtcp.PictureLossIndication).MediaSSRC
				if p.pliThrottle.canSend(mediaSSRC) {
					fwdPkts = append(fwdPkts, pkt)
				}
			case *rtcp.FullIntraRequest:
				mediaSSRC := pkt.(*rtcp.FullIntraRequest).MediaSSRC
				if p.pliThrottle.canSend(mediaSSRC) {
					fwdPkts = append(fwdPkts, pkt)
				}
			default:
				fwdPkts = append(fwdPkts, pkt)
			}
		}

		if len(fwdPkts) > 0 {
			if err := p.publisher.pc.WriteRTCP(fwdPkts); err != nil {
				p.params.Logger.Errorw("could not write RTCP to participant", err,
					"participant", p.Identity(), "pID", p.ID())
			}
		}
	}
}

func (p *ParticipantImpl) configureReceiverDTX() {
	//
	// DTX (Discontinuous Transmission) allows audio bandwidth saving
	// by not sending packets during silence periods.
	//
	// Publisher side DTX can enabled by included `usedtx=1` in
	// the `fmtp` line corresponding to audio codec (Opus) in SDP.
	// By doing this in the SDP `answer`, it can be controlled from
	// server side and avoid doing it in all the client SDKs.
	//
	// Ideally, a publisher should be able to specify per audio
	// track if DTX should be enabled. But, translating the
	// DTX preference of publisher to the correct transceiver
	// is non-deterministic due to the lack of a synchronizing id
	// like the track id. The codec preference to set DTX needs
	// to be done
	//   - after calling `SetRemoteDescription` which sets up
	//     the transceivers, but there are no tracks in the
	//     transceiver yet
	//   - before calling `CreateAnswer`
	// Due to the absensce of tracks when it is required to set DTX,
	// it is not possible to cross reference against a pending track
	// with the same track id.
	//
	// Due to the restriction above and given that in practice
	// most of the time there is going to be only one audio track
	// that is published, do the following
	//    - if there is no pending audio track, no-op
	//    - if there are no audio transceivers without tracks, no-op
	//    - else, apply the DTX setting from pending audio track
	//      to the audio transceiver without no tracks
	//
	// NOTE: The above logic will fail if there is an `offer` SDP with
	// multiple audio tracks. At that point, there might be a need to
	// rely on something like order of tracks. TODO
	//
	enableDTX := false

	p.lock.RLock()
	var pendingTrack *livekit.TrackInfo
	for _, track := range p.pendingTracks {
		if track.Type == livekit.TrackType_AUDIO {
			pendingTrack = track
			break
		}
	}

	if pendingTrack == nil {
		p.lock.RUnlock()
		return
	}

	enableDTX = !pendingTrack.DisableDtx
	p.lock.RUnlock()

	transceivers := p.publisher.pc.GetTransceivers()
	for _, transceiver := range transceivers {
		if transceiver.Kind() != webrtc.RTPCodecTypeAudio {
			continue
		}

		receiver := transceiver.Receiver()
		if receiver == nil || receiver.Track() != nil {
			continue
		}

		var modifiedReceiverCodecs []webrtc.RTPCodecParameters

		receiverCodecs := receiver.GetParameters().Codecs
		for _, receiverCodec := range receiverCodecs {
			if receiverCodec.MimeType == webrtc.MimeTypeOpus {
				fmtpUseDTX := "usedtx=1"
				// remove occurrence in the middle
				sdpFmtpLine := strings.ReplaceAll(receiverCodec.SDPFmtpLine, fmtpUseDTX+";", "")
				// remove occurrence at the end
				sdpFmtpLine = strings.ReplaceAll(sdpFmtpLine, fmtpUseDTX, "")
				if enableDTX {
					sdpFmtpLine += ";" + fmtpUseDTX
				}
				receiverCodec.SDPFmtpLine = sdpFmtpLine
			}
			modifiedReceiverCodecs = append(modifiedReceiverCodecs, receiverCodec)
		}

		//
		// As `SetCodecPreferences` on a transceiver replaces all codecs,
		// cycle through sender codecs also and add them before calling
		// `SetCodecPreferences`
		//
		var senderCodecs []webrtc.RTPCodecParameters
		sender := transceiver.Sender()
		if sender != nil {
			senderCodecs = sender.GetParameters().Codecs
		}

		err := transceiver.SetCodecPreferences(append(modifiedReceiverCodecs, senderCodecs...))
		if err != nil {
			p.params.Logger.Warnw("failed to SetCodecPreferences", err)
		}
	}
}

func (p *ParticipantImpl) onStreamedTracksChange(update *sfu.StreamedTracksUpdate) error {
	if len(update.Paused) == 0 && len(update.Resumed) == 0 {
		return nil
	}

	streamedTracksUpdate := &livekit.StreamedTracksUpdate{}
	for _, streamedTrack := range update.Paused {
		streamedTracksUpdate.Paused = append(streamedTracksUpdate.Paused, &livekit.StreamedTrack{
			ParticipantSid: streamedTrack.ParticipantSid,
			TrackSid:       streamedTrack.TrackSid,
		})
	}
	for _, streamedTrack := range update.Resumed {
		streamedTracksUpdate.Resumed = append(streamedTracksUpdate.Resumed, &livekit.StreamedTrack{
			ParticipantSid: streamedTrack.ParticipantSid,
			TrackSid:       streamedTrack.TrackSid,
		})
	}

	return p.writeMessage(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_StreamedTracksUpdate{
			StreamedTracksUpdate: streamedTracksUpdate,
		},
	})
}

func (p *ParticipantImpl) DebugInfo() map[string]interface{} {
	info := map[string]interface{}{
		"ID":    p.id,
		"State": p.State().String(),
	}

	publishedTrackInfo := make(map[string]interface{})
	subscribedTrackInfo := make(map[string]interface{})
	pendingTrackInfo := make(map[string]interface{})

	p.lock.RLock()
	for trackID, track := range p.publishedTracks {
		if mt, ok := track.(*MediaTrack); ok {
			publishedTrackInfo[trackID] = mt.DebugInfo()
		} else {
			publishedTrackInfo[trackID] = map[string]interface{}{
				"ID":       track.ID(),
				"Kind":     track.Kind().String(),
				"PubMuted": track.IsMuted(),
			}
		}
	}

	for _, track := range p.subscribedTracks {
		dt := track.DownTrack().DebugInfo()
		dt["SubMuted"] = track.IsMuted()
		subscribedTrackInfo[track.ID()] = dt
	}

	for clientID, track := range p.pendingTracks {
		pendingTrackInfo[clientID] = map[string]interface{}{
			"Sid":       track.Sid,
			"Type":      track.Type.String(),
			"Simulcast": track.Simulcast,
		}
	}
	p.lock.RUnlock()

	info["PublishedTracks"] = publishedTrackInfo
	info["SubscribedTracks"] = subscribedTrackInfo
	info["PendingTracks"] = pendingTrackInfo

	return info
}
