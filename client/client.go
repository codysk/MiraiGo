package client

import (
	"crypto/md5"
	"errors"
	"fmt"
	"github.com/Mrs4s/MiraiGo/binary"
	"github.com/Mrs4s/MiraiGo/client/pb/longmsg"
	"github.com/Mrs4s/MiraiGo/client/pb/msg"
	"github.com/Mrs4s/MiraiGo/client/pb/multimsg"
	"github.com/Mrs4s/MiraiGo/message"
	"github.com/Mrs4s/MiraiGo/protocol/packets"
	"github.com/Mrs4s/MiraiGo/utils"
	"github.com/golang/protobuf/proto"
	"io"
	"log"
	"math"
	"math/rand"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type QQClient struct {
	Uin         int64
	PasswordMd5 [16]byte

	Nickname   string
	Age        uint16
	Gender     uint16
	FriendList []*FriendInfo
	GroupList  []*GroupInfo
	Online     bool

	SequenceId              uint16
	OutGoingPacketSessionId []byte
	RandomKey               []byte
	Conn                    net.Conn

	decoders map[string]func(*QQClient, uint16, []byte) (interface{}, error)
	handlers sync.Map

	syncCookie       []byte
	pubAccountCookie []byte
	msgCtrlBuf       []byte
	ksid             []byte
	t104             []byte
	t150             []byte
	t149             []byte
	t528             []byte
	t530             []byte
	rollbackSig      []byte
	timeDiff         int64
	sigInfo          *loginSigInfo
	pwdFlag          bool

	lastMessageSeq         int32
	lastMessageSeqTmp      sync.Map
	lastLostMsg            string
	groupMsgBuilders       sync.Map
	onlinePushCache        []int16 // reset on reconnect
	requestPacketRequestId int32
	groupSeq               int32
	friendSeq              int32
	groupDataTransSeq      int32
	highwayApplyUpSeq      int32
	eventHandlers          *eventHandlers

	groupListLock *sync.Mutex
}

type loginSigInfo struct {
	loginBitmap uint64
	tgt         []byte
	tgtKey      []byte

	userStKey          []byte
	userStWebSig       []byte
	sKey               []byte
	d2                 []byte
	d2Key              []byte
	wtSessionTicketKey []byte
	deviceToken        []byte
}

func init() {
	rand.Seed(time.Now().UTC().UnixNano())
}

// NewClient create new qq client
func NewClient(uin int64, password string) *QQClient {
	return NewClientMd5(uin, md5.Sum([]byte(password)))
}

func NewClientMd5(uin int64, passwordMd5 [16]byte) *QQClient {
	cli := &QQClient{
		Uin:                     uin,
		PasswordMd5:             passwordMd5,
		SequenceId:              0x3635,
		RandomKey:               make([]byte, 16),
		OutGoingPacketSessionId: []byte{0x02, 0xB0, 0x5B, 0x8B},
		decoders: map[string]func(*QQClient, uint16, []byte) (interface{}, error){
			"wtlogin.login":                            decodeLoginResponse,
			"StatSvc.register":                         decodeClientRegisterResponse,
			"StatSvc.ReqMSFOffline":                    decodeMSFOfflinePacket,
			"MessageSvc.PushNotify":                    decodeSvcNotify,
			"OnlinePush.PbPushGroupMsg":                decodeGroupMessagePacket,
			"OnlinePush.ReqPush":                       decodeOnlinePushReqPacket,
			"OnlinePush.PbPushTransMsg":                decodeOnlinePushTransPacket,
			"ConfigPushSvc.PushReq":                    decodePushReqPacket,
			"MessageSvc.PbGetMsg":                      decodeMessageSvcPacket,
			"MessageSvc.PushForceOffline":              decodeForceOfflinePacket,
			"friendlist.getFriendGroupList":            decodeFriendGroupListResponse,
			"friendlist.GetTroopListReqV2":             decodeGroupListResponse,
			"friendlist.GetTroopMemberListReq":         decodeGroupMemberListResponse,
			"ImgStore.GroupPicUp":                      decodeGroupImageStoreResponse,
			"LongConn.OffPicUp":                        decodeOffPicUpResponse,
			"ProfileService.Pb.ReqSystemMsgNew.Group":  decodeSystemMsgGroupPacket,
			"ProfileService.Pb.ReqSystemMsgNew.Friend": decodeSystemMsgFriendPacket,
			"MultiMsg.ApplyUp":                         decodeMultiApplyUpResponse,
			"MultiMsg.ApplyDown":                       decodeMultiApplyDownResponse,
		},
		sigInfo:                &loginSigInfo{},
		requestPacketRequestId: 1921334513,
		groupSeq:               22911,
		friendSeq:              22911,
		highwayApplyUpSeq:      77918,
		ksid:                   []byte("|454001228437590|A8.2.7.27f6ea96"),
		eventHandlers:          &eventHandlers{},
		groupListLock:          new(sync.Mutex),
	}
	rand.Read(cli.RandomKey)
	return cli
}

// Login send login request
func (c *QQClient) Login() (*LoginResponse, error) {
	if c.Online {
		return nil, ErrAlreadyOnline
	}
	err := c.connect()
	if err != nil {
		return nil, err
	}
	c.Online = true
	go c.loop()
	seq, packet := c.buildLoginPacket()
	rsp, err := c.sendAndWait(seq, packet)
	if err != nil {
		return nil, err
	}
	l := rsp.(LoginResponse)
	if l.Success {
		c.lastLostMsg = ""
		c.registerClient()
		go c.heartbeat()
		_, _ = c.sendAndWait(c.buildGetMessageRequestPacket(msg.SyncFlag_START, time.Now().Unix()))
	}
	return &l, nil
}

// SubmitCaptcha send captcha to server
func (c *QQClient) SubmitCaptcha(result string, sign []byte) (*LoginResponse, error) {
	seq, packet := c.buildCaptchaPacket(result, sign)
	rsp, err := c.sendAndWait(seq, packet)
	if err != nil {
		return nil, err
	}
	l := rsp.(LoginResponse)
	if l.Success {
		c.registerClient()
		go c.heartbeat()
	}
	return &l, nil
}

// ReloadFriendList refresh QQClient.FriendList field via GetFriendList()
func (c *QQClient) ReloadFriendList() error {
	rsp, err := c.GetFriendList()
	if err != nil {
		return err
	}
	c.FriendList = rsp.List
	return nil
}

// GetFriendList request friend list
func (c *QQClient) GetFriendList() (*FriendListResponse, error) {
	var curFriendCount = 0
	r := &FriendListResponse{}
	for {
		rsp, err := c.sendAndWait(c.buildFriendGroupListRequestPacket(int16(curFriendCount), 150, 0, 0))
		if err != nil {
			return nil, err
		}
		list := rsp.(FriendListResponse)
		r.TotalCount = list.TotalCount
		r.List = append(r.List, list.List...)
		curFriendCount += len(list.List)
		if int32(curFriendCount) >= r.TotalCount {
			break
		}
	}
	return r, nil
}

func (c *QQClient) SendGroupMessage(groupCode int64, m *message.SendingMessage) *message.GroupMessage {
	imgCount := m.Count(func(e message.IMessageElement) bool { return e.Type() == message.Image })
	msgLen := message.EstimateLength(m.Elements, 703)
	if msgLen > 5000 || imgCount > 50 {
		return nil
	}
	if msgLen > 702 || imgCount > 2 {
		return c.sendGroupLongOrForwardMessage(groupCode, true, &message.ForwardMessage{Nodes: []*message.ForwardNode{
			{
				SenderId:   c.Uin,
				SenderName: c.Nickname,
				Time:       int32(time.Now().Unix()),
				Message:    m.Elements,
			},
		}})
	}
	return c.sendGroupMessage(groupCode, false, m)
}

func (c *QQClient) sendGroupMessage(groupCode int64, forward bool, m *message.SendingMessage) *message.GroupMessage {
	eid := utils.RandomString(6)
	mr := int32(rand.Uint32())
	ch := make(chan int32)
	c.onGroupMessageReceipt(eid, func(c *QQClient, e *groupMessageReceiptEvent) {
		if e.Rand == mr {
			ch <- e.Seq
		}
	})
	defer c.onGroupMessageReceipt(eid)
	_, pkt := c.buildGroupSendingPacket(groupCode, mr, forward, m)
	_ = c.send(pkt)
	var mid int32
	ret := &message.GroupMessage{
		Id:         -1,
		InternalId: mr,
		GroupCode:  groupCode,
		Sender: &message.Sender{
			Uin:      c.Uin,
			Nickname: c.Nickname,
			IsFriend: true,
		},
		Time:     int32(time.Now().Unix()),
		Elements: m.Elements,
	}
	select {
	case mid = <-ch:
	case <-time.After(time.Second * 5):
		return ret
	}
	ret.Id = mid
	return ret
}

func (c *QQClient) SendPrivateMessage(target int64, m *message.SendingMessage) *message.PrivateMessage {
	mr := int32(rand.Uint32())
	seq := c.nextFriendSeq()
	t := time.Now().Unix()
	_, pkt := c.buildFriendSendingPacket(target, seq, mr, t, m)
	_ = c.send(pkt)
	return &message.PrivateMessage{
		Id:         seq,
		InternalId: mr,
		Target:     target,
		Time:       int32(t),
		Sender: &message.Sender{
			Uin:      c.Uin,
			Nickname: c.Nickname,
			IsFriend: true,
		},
		Elements: m.Elements,
	}
}

func (c *QQClient) GetForwardMessage(resId string) *message.ForwardMessage {
	i, err := c.sendAndWait(c.buildMultiApplyDownPacket(resId))
	if err != nil {
		return nil
	}
	multiMsg := i.(*msg.PbMultiMsgTransmit)
	ret := &message.ForwardMessage{}
	for _, m := range multiMsg.Msg {
		ret.Nodes = append(ret.Nodes, &message.ForwardNode{
			SenderId: m.Head.FromUin,
			SenderName: func() string {
				if m.Head.MsgType == 82 {
					return m.Head.GroupInfo.GroupCard
				}
				return m.Head.FromNick
			}(),
			Time:    m.Head.MsgTime,
			Message: message.ParseMessageElems(m.Body.RichText.Elems),
		})
	}
	return ret
}

func (c *QQClient) SendGroupForwardMessage(groupCode int64, m *message.ForwardMessage) *message.GroupMessage {
	return c.sendGroupLongOrForwardMessage(groupCode, false, m)
}

func (c *QQClient) sendGroupLongOrForwardMessage(groupCode int64, isLong bool, m *message.ForwardMessage) *message.GroupMessage {
	if len(m.Nodes) >= 200 {
		return nil
	}
	ts := time.Now().Unix()
	seq := c.nextGroupSeq()
	data, hash := m.CalculateValidationData(seq, rand.Int31(), groupCode)
	i, err := c.sendAndWait(c.buildMultiApplyUpPacket(data, hash, func() int32 {
		if isLong {
			return 1
		} else {
			return 2
		}
	}(), utils.ToGroupUin(groupCode)))
	if err != nil {
		return nil
	}
	rsp := i.(*multimsg.MultiMsgApplyUpRsp)
	body, _ := proto.Marshal(&longmsg.LongReqBody{
		Subcmd:       1,
		TermType:     5,
		PlatformType: 9,
		MsgUpReq: []*longmsg.LongMsgUpReq{
			{
				MsgType:    3,
				DstUin:     utils.ToGroupUin(groupCode),
				MsgContent: data,
				StoreType:  2,
				MsgUkey:    rsp.MsgUkey,
			},
		},
	})
	for i, ip := range rsp.Uint32UpIp {
		updServer := binary.UInt32ToIPV4Address(uint32(ip))
		err := c.highwayUploadImage(updServer+":"+strconv.FormatInt(int64(rsp.Uint32UpPort[i]), 10), rsp.MsgSig, body, 27)
		if err == nil {
			if !isLong {
				var pv string
				for i := 0; i < int(math.Min(4, float64(len(m.Nodes)))); i++ {
					pv += fmt.Sprintf(`<title size="26" color="#777777">%s: %s</title>`, m.Nodes[i].SenderName, message.ToReadableString(m.Nodes[i].Message))
				}
				return c.sendGroupMessage(groupCode, true, genForwardTemplate(rsp.MsgResid, pv, "群聊的聊天记录", "[聊天记录]", "聊天记录", fmt.Sprintf("查看 %d 条转发消息", len(m.Nodes)), ts))
			}
			bri := func() string {
				var r string
				for _, n := range m.Nodes {
					r += message.ToReadableString(n.Message)
					if len(r) >= 27 {
						break
					}
				}
				return r
			}()
			return c.sendGroupMessage(groupCode, false, genLongTemplate(rsp.MsgResid, bri, ts))
		}
	}
	return nil
}

func (c *QQClient) RecallGroupMessage(groupCode int64, msgId, msgInternalId int32) {
	_, pkt := c.buildGroupRecallPacket(groupCode, msgId, msgInternalId)
	_ = c.send(pkt)
}

func (c *QQClient) UploadGroupImage(groupCode int64, img []byte) (*message.GroupImageElement, error) {
	h := md5.Sum(img)
	seq, pkt := c.buildGroupImageStorePacket(groupCode, h[:], int32(len(img)))
	r, err := c.sendAndWait(seq, pkt)
	if err != nil {
		return nil, err
	}
	rsp := r.(imageUploadResponse)
	if rsp.ResultCode != 0 {
		return nil, errors.New(rsp.Message)
	}
	if rsp.IsExists {
		return message.NewGroupImage(binary.CalculateImageResourceId(h[:]), h[:]), nil
	}
	for i, ip := range rsp.UploadIp {
		updServer := binary.UInt32ToIPV4Address(uint32(ip))
		err := c.highwayUploadImage(updServer+":"+strconv.FormatInt(int64(rsp.UploadPort[i]), 10), rsp.UploadKey, img, 2)
		if err != nil {
			continue
		}
		return message.NewGroupImage(binary.CalculateImageResourceId(h[:]), h[:]), nil
	}
	return nil, errors.New("upload failed")
}

func (c *QQClient) UploadPrivateImage(target int64, img []byte) (*message.FriendImageElement, error) {
	return c.uploadPrivateImage(target, img, 0)
}

func (c *QQClient) uploadPrivateImage(target int64, img []byte, count int) (*message.FriendImageElement, error) {
	count++
	h := md5.Sum(img)
	e, err := c.QueryFriendImage(target, h[:], int32(len(img)))
	if err != nil {
		// use group highway upload and query again for image id.
		if _, err = c.UploadGroupImage(target, img); err != nil {
			return nil, err
		}
		// safe
		if count >= 5 {
			return nil, errors.New("upload failed")
		}
		return c.uploadPrivateImage(target, img, count)
	}
	return e, nil
}

func (c *QQClient) QueryGroupImage(groupCode int64, hash []byte, size int32) (*message.GroupImageElement, error) {
	r, err := c.sendAndWait(c.buildGroupImageStorePacket(groupCode, hash, size))
	if err != nil {
		return nil, err
	}
	rsp := r.(imageUploadResponse)
	if rsp.ResultCode != 0 {
		return nil, errors.New(rsp.Message)
	}
	if rsp.IsExists {
		return message.NewGroupImage(binary.CalculateImageResourceId(hash), hash), nil
	}
	return nil, errors.New("image not exists")
}

func (c *QQClient) QueryFriendImage(target int64, hash []byte, size int32) (*message.FriendImageElement, error) {
	i, err := c.sendAndWait(c.buildOffPicUpPacket(target, hash, size))
	if err != nil {
		return nil, err
	}
	rsp := i.(imageUploadResponse)
	if rsp.ResultCode != 0 {
		return nil, errors.New(rsp.Message)
	}
	if !rsp.IsExists {
		return nil, errors.New("image not exists")
	}
	return &message.FriendImageElement{
		ImageId: rsp.ResourceId,
		Md5:     hash,
	}, nil
}

func (c *QQClient) ReloadGroupList() error {
	c.groupListLock.Lock()
	defer c.groupListLock.Unlock()
	list, err := c.GetGroupList()
	if err != nil {
		return err
	}
	c.GroupList = list
	return nil
}

func (c *QQClient) GetGroupList() ([]*GroupInfo, error) {
	rsp, err := c.sendAndWait(c.buildGroupListRequestPacket())
	if err != nil {
		return nil, err
	}
	r := rsp.([]*GroupInfo)
	for _, group := range r {
		m, err := c.GetGroupMembers(group)
		if err != nil {
			continue
		}
		group.Members = m
	}
	return r, nil
}

func (c *QQClient) GetGroupMembers(group *GroupInfo) ([]*GroupMemberInfo, error) {
	var nextUin int64
	var list []*GroupMemberInfo
	for {
		data, err := c.sendAndWait(c.buildGroupMemberListRequestPacket(group.Uin, group.Code, nextUin))
		if err != nil {
			return nil, err
		}
		rsp := data.(groupMemberListResponse)
		nextUin = rsp.NextUin
		for _, m := range rsp.list {
			m.Group = group
			if m.Uin == group.OwnerUin {
				m.Permission = Owner
			}
		}
		list = append(list, rsp.list...)
		if nextUin == 0 {
			return list, nil
		}
	}
}

func (c *QQClient) FindFriend(uin int64) *FriendInfo {
	for _, t := range c.FriendList {
		f := t
		if f.Uin == uin {
			return f
		}
	}
	return nil
}

func (c *QQClient) FindGroupByUin(uin int64) *GroupInfo {
	for _, g := range c.GroupList {
		f := g
		if f.Uin == uin {
			return f
		}
	}
	return nil
}

func (c *QQClient) FindGroup(code int64) *GroupInfo {
	for _, g := range c.GroupList {
		f := g
		if f.Code == code {
			return f
		}
	}
	return nil
}

func (c *QQClient) SolveGroupJoinRequest(i interface{}, accept bool) {
	switch req := i.(type) {
	case *UserJoinGroupRequest:
		_, pkt := c.buildSystemMsgGroupActionPacket(req.RequestId, req.RequesterUin, req.GroupCode, false, accept, false)
		_ = c.send(pkt)
	case *GroupInvitedRequest:
		_, pkt := c.buildSystemMsgGroupActionPacket(req.RequestId, req.InvitorUin, req.GroupCode, true, accept, false)
		_ = c.send(pkt)
	}
}

func (c *QQClient) SolveFriendRequest(req *NewFriendRequest, accept bool) {
	_, pkt := c.buildSystemMsgFriendActionPacket(req.RequestId, req.RequesterUin, accept)
	_ = c.send(pkt)
}

func (g *GroupInfo) SelfPermission() MemberPermission {
	return g.FindMember(g.client.Uin).Permission
}

func (g *GroupInfo) AdministratorOrOwner() bool {
	return g.SelfPermission() == Administrator || g.SelfPermission() == Owner
}

func (g *GroupInfo) FindMember(uin int64) *GroupMemberInfo {
	for _, m := range g.Members {
		f := m
		if f.Uin == uin {
			return f
		}
	}
	return nil
}

func (c *QQClient) editMemberCard(groupCode, memberUin int64, card string) {
	_, _ = c.sendAndWait(c.buildEditGroupTagPacket(groupCode, memberUin, card))
}

func (c *QQClient) editMemberSpecialTitle(groupCode, memberUin int64, title string) {
	_, _ = c.sendAndWait(c.buildEditSpecialTitlePacket(groupCode, memberUin, title))
}

func (c *QQClient) updateGroupName(groupCode int64, newName string) {
	_, _ = c.sendAndWait(c.buildGroupNameUpdatePacket(groupCode, newName))
}

func (c *QQClient) groupMuteAll(groupCode int64, mute bool) {
	_, _ = c.sendAndWait(c.buildGroupMuteAllPacket(groupCode, mute))
}

func (c *QQClient) groupMute(groupCode, memberUin int64, time uint32) {
	_, _ = c.sendAndWait(c.buildGroupMutePacket(groupCode, memberUin, time))
}

func (c *QQClient) quitGroup(groupCode int64) {
	_, _ = c.sendAndWait(c.buildQuitGroupPacket(groupCode))
}

func (c *QQClient) kickGroupMember(groupCode, memberUin int64, msg string) {
	_, _ = c.sendAndWait(c.buildGroupKickPacket(groupCode, memberUin, msg))
}

func (g *GroupInfo) removeMember(uin int64) {
	if g.memLock == nil {
		g.memLock = new(sync.Mutex)
	}
	g.memLock.Lock()
	defer g.memLock.Unlock()
	for i, m := range g.Members {
		if m.Uin == uin {
			g.Members = append(g.Members[:i], g.Members[i+1:]...)
			break
		}
	}
}

func (c *QQClient) connect() error {
	conn, err := net.Dial("tcp", "125.94.60.146:80") //TODO: more servers
	if err != nil {
		return err
	}
	c.Conn = conn
	c.onlinePushCache = []int16{}
	return nil
}

func (c *QQClient) registerClient() {
	_, packet := c.buildClientRegisterPacket()
	_ = c.send(packet)
}

func (c *QQClient) nextSeq() uint16 {
	c.SequenceId++
	c.SequenceId &= 0x7FFF
	if c.SequenceId == 0 {
		c.SequenceId++
	}
	return c.SequenceId
}

func (c *QQClient) nextPacketSeq() int32 {
	s := atomic.LoadInt32(&c.requestPacketRequestId)
	atomic.AddInt32(&c.requestPacketRequestId, 2)
	return s
}

func (c *QQClient) nextGroupSeq() int32 {
	s := atomic.LoadInt32(&c.groupSeq)
	atomic.AddInt32(&c.groupSeq, 2)
	return s
}

func (c *QQClient) nextFriendSeq() int32 {
	s := atomic.LoadInt32(&c.friendSeq)
	atomic.AddInt32(&c.friendSeq, 1)
	return s
}

func (c *QQClient) nextGroupDataTransSeq() int32 {
	s := atomic.LoadInt32(&c.groupDataTransSeq)
	atomic.AddInt32(&c.groupDataTransSeq, 2)
	return s
}

func (c *QQClient) nextHighwayApplySeq() int32 {
	s := atomic.LoadInt32(&c.highwayApplyUpSeq)
	atomic.AddInt32(&c.highwayApplyUpSeq, 2)
	return s
}

func (c *QQClient) send(pkt []byte) error {
	_, err := c.Conn.Write(pkt)
	return err
}

func (c *QQClient) sendAndWait(seq uint16, pkt []byte) (interface{}, error) {
	type T struct {
		Response interface{}
		Error    error
	}
	_, err := c.Conn.Write(pkt)
	if err != nil {
		return nil, err
	}
	ch := make(chan T)
	defer close(ch)
	c.handlers.Store(seq, func(i interface{}, err error) {
		ch <- T{
			Response: i,
			Error:    err,
		}
	})
	select {
	case rsp := <-ch:
		return rsp.Response, rsp.Error
	case <-time.After(time.Second * 15):
		c.handlers.Delete(seq)
		return nil, errors.New("time out")
	}
}

func (c *QQClient) loop() {
	reader := binary.NewNetworkReader(c.Conn)
	retry := 0
	for c.Online {
		l, err := reader.ReadInt32()
		if err == io.EOF || err == io.ErrClosedPipe {
			err = c.connect()
			if err != nil {
				break
			}
			reader = binary.NewNetworkReader(c.Conn)
			c.registerClient()
		}
		if l <= 0 {
			retry++
			time.Sleep(time.Second * 3)
			if retry > 10 {
				c.Online = false
			}
			continue
		}
		data, err := reader.ReadBytes(int(l) - 4)
		pkt, err := packets.ParseIncomingPacket(data, c.sigInfo.d2Key)
		if err != nil {
			log.Println("parse incoming packet error: " + err.Error())
			continue
		}
		payload := pkt.Payload
		if pkt.Flag2 == 2 {
			payload, err = pkt.DecryptPayload(c.RandomKey)
			if err != nil {
				continue
			}
		}
		retry = 0
		//fmt.Println(pkt.CommandName)
		go func() {
			defer func() {
				if pan := recover(); pan != nil {
					//
				}
			}()
			decoder, ok := c.decoders[pkt.CommandName]
			if !ok {
				if f, ok := c.handlers.Load(pkt.SequenceId); ok {
					c.handlers.Delete(pkt.SequenceId)
					f.(func(i interface{}, err error))(nil, nil)
				}
				return
			}
			rsp, err := decoder(c, pkt.SequenceId, payload)
			if err != nil {
				log.Println("decode", pkt.CommandName, "error:", err)
			}
			if f, ok := c.handlers.Load(pkt.SequenceId); ok {
				c.handlers.Delete(pkt.SequenceId)
				f.(func(i interface{}, err error))(rsp, err)
			}
		}()
	}
	_ = c.Conn.Close()
	if c.lastLostMsg == "" {
		c.lastLostMsg = "Connection lost."
	}
	c.dispatchDisconnectEvent(&ClientDisconnectedEvent{Message: c.lastLostMsg})
}

func (c *QQClient) heartbeat() {
	for c.Online {
		time.Sleep(time.Second * 30)
		seq := c.nextSeq()
		sso := packets.BuildSsoPacket(seq, "Heartbeat.Alive", SystemDeviceInfo.IMEI, []byte{}, c.OutGoingPacketSessionId, []byte{}, c.ksid)
		packet := packets.BuildLoginPacket(c.Uin, 0, []byte{}, sso, []byte{})
		_, _ = c.sendAndWait(seq, packet)
	}
}
