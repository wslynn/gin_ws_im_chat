package service

import (
	"chat/conf"
	"chat/ret"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"log"
	"net/http"
	"time"
)

const month = 60 * 60 * 24 * 30

type SendMsg struct {
	Type    int    `json:"type"`
	Content string `json:"content"`
}

type ReplyMsg struct {
	From    string `json:"from"`
	Code    int    `json:"code"`
	Content string `json:"content"`
}

type Client struct { // 用户类
	ID     string
	SendID string
	WsConn *websocket.Conn
	Send   chan []byte
}

// 接收消息
func (this *Client) Read() {
	defer func() {
		Manager.Offline <- this
		_ = this.WsConn.Close()
	}()
	for {
		this.WsConn.PongHandler()
		sendMsg := new(SendMsg)
		err := this.WsConn.ReadJSON(&sendMsg)
		if err != nil {
			log.Println("数据格式不正确", err)
			break
		}
		if sendMsg.Type == 1 { // 发送消息
			r1, _ := conf.RedisClient.Get(this.ID).Result()
			r2, _ := conf.RedisClient.Get(this.SendID).Result()
			if r1 > "3" && r2 == "" { // 防止1频繁骚扰2
				replyMsg := ReplyMsg{
					Code:    ret.WebsocketLimit,
					Content: "达到限制",
				}
				msg, _ := json.Marshal(replyMsg)
				_ = this.WsConn.WriteMessage(websocket.TextMessage, msg)
				continue
			} else {
				conf.RedisClient.Incr(this.ID)
				// 建立的连接缓存三个月
				_, _ = conf.RedisClient.Expire(this.ID, time.Hour*24*30*3).Result()
			}
			Manager.Broadcast <- &Broadcast{Client: this, Message: []byte(sendMsg.Content)}
		} else if sendMsg.Type == 2 { // 获取历史消息
			// 获取10条历史消息
			results, _ := FindMany(conf.MongoDBName, this.SendID, this.ID, 10)
			if len(results) > 10 {
				results = results[:10]
			} else if len(results) == 0 {
				replyMsg := ReplyMsg{
					Code:    ret.WebsocketEnd,
					Content: "到底了",
				}
				msg, _ := json.Marshal(replyMsg)
				_ = this.WsConn.WriteMessage(websocket.TextMessage, msg)
				continue
			}
			for _, result := range results {
				replyMsg := ReplyMsg{
					From:    result.From,
					Content: fmt.Sprintf("%s", result.Msg),
				}
				msg, _ := json.Marshal(replyMsg)
				_ = this.WsConn.WriteMessage(websocket.TextMessage, msg)
			}
		} else if sendMsg.Type == 3 { // 首次查询时, 取出对方发来的所有未读
			results, err := FirstFindMsg(conf.MongoDBName, this.SendID, this.ID)
			if err != nil {
				log.Println(err)
			}
			fmt.Println("首次查询:", results)
			for _, result := range results {
				replyMsg := ReplyMsg{
					From:    result.From,
					Content: fmt.Sprintf("%s", result.Msg),
				}
				msg, _ := json.Marshal(replyMsg)
				_ = this.WsConn.WriteMessage(websocket.TextMessage, msg)
			}
		}
	}
}

// 发送消息
func (this *Client) Write() {
	defer func() {
		_ = this.WsConn.Close()
	}()
	for {
		select {
		case message, ok := <-this.Send:
			if !ok {
				_ = this.WsConn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			log.Println(this.ID, "接受消息:", string(message))
			replyMsg := ReplyMsg{
				Code:    ret.WebsocketSuccessMessage,
				Content: fmt.Sprintf("%s", string(message)),
			}
			msg, _ := json.Marshal(replyMsg)
			_ = this.WsConn.WriteMessage(websocket.TextMessage, msg)
		}
	}
}

// 广播类，包括广播内容和源用户
type Broadcast struct {
	Client  *Client // 信息是哪个用户发送的
	Message []byte  // 信息的内容
	Type    int     // 信息的类型
}

// 用户管理
type ClientManager struct {
	Clients   map[string]*Client
	Broadcast chan *Broadcast
	Reply     chan *Client
	Online    chan *Client // 在线的client
	Offline   chan *Client // 离线的client
}

func (this *ClientManager) Start() {
	for {
		log.Println("<---监听管道通信--->")
		select {
		case client := <-Manager.Online: // 建立连接
			log.Printf("建立新连接: %v", client.ID)
			Manager.Clients[client.ID] = client
			replyMsg := &ReplyMsg{
				Code:    ret.WebsocketSuccess,
				Content: "已连接至服务器",
			}
			msg, _ := json.Marshal(replyMsg)
			_ = client.WsConn.WriteMessage(websocket.TextMessage, msg)
		case client := <-Manager.Offline: // 断开连接
			log.Printf("连接失败:%v", client.ID)
			if _, ok := Manager.Clients[client.ID]; ok {
				replyMsg := &ReplyMsg{
					Code:    ret.WebsocketEnd,
					Content: "连接已断开",
				}
				msg, _ := json.Marshal(replyMsg)
				_ = client.WsConn.WriteMessage(websocket.TextMessage, msg)
				close(client.Send)
				delete(Manager.Clients, client.ID)
			}
		case broadcast := <-Manager.Broadcast: // 广播信息
			message := broadcast.Message
			sendId := broadcast.Client.SendID
			flag := false // 默认对方不在线
			for id, client := range Manager.Clients {
				if id != sendId {
					continue
				}
				select {
				case client.Send <- message:
					flag = true
				default:
					close(client.Send)
					delete(Manager.Clients, client.ID)
				}
			}
			id := broadcast.Client.ID
			if flag {
				log.Println("对方在线应答")
				replyMsg := &ReplyMsg{
					Code:    ret.WebsocketOnlineReply,
					Content: "对方在线应答",
				}
				msg, err := json.Marshal(replyMsg)
				_ = broadcast.Client.WsConn.WriteMessage(websocket.TextMessage, msg)
				err = InsertMsg(conf.MongoDBName, id, string(message), 1, int64(3*month))
				if err != nil {
					fmt.Println("InsertOneMsg Err", err)
				}
			} else {
				log.Println("对方不在线")
				replyMsg := ReplyMsg{
					Code:    ret.WebsocketOfflineReply,
					Content: "对方不在线应答",
				}
				msg, err := json.Marshal(replyMsg)
				_ = broadcast.Client.WsConn.WriteMessage(websocket.TextMessage, msg)
				err = InsertMsg(conf.MongoDBName, id, string(message), 0, int64(3*month))
				if err != nil {
					fmt.Println("InsertOneMsg Err", err)
				}
			}
		}
	}
}

// Message 信息转JSON (包括：发送者、接收者、内容)
type Message struct {
	Sender    string `json:"sender,omitempty"`
	Recipient string `json:"recipient,omitempty"`
	Content   string `json:"content,omitempty"`
}

var Manager = ClientManager{
	Clients:   make(map[string]*Client), // 参与连接的用户，出于性能的考虑，需要设置最大连接数
	Broadcast: make(chan *Broadcast),
	Online:    make(chan *Client),
	Reply:     make(chan *Client),
	Offline:   make(chan *Client),
}

func CreateID(uid, toUid string) string {
	return uid + "->" + toUid
}

func Handler(c *gin.Context) {
	uid := c.Query("uid")
	toUid := c.Query("toUid")
	var upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { // CheckOrigin解决跨域问题
			return true
		},
	}
	// 调用Upgrader.Upgrade使http协议, 升级成ws协议, 并返回一个*conn
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		http.NotFound(c.Writer, c.Request)
		return
	}
	// 创建一个用户实例
	client := &Client{
		ID:     CreateID(uid, toUid),
		SendID: CreateID(toUid, uid),
		WsConn: conn,
		Send:   make(chan []byte),
	}
	// 用户注册到用户管理上
	Manager.Online <- client
	// 使用conn收发消息
	go client.Read()
	go client.Write()
}
