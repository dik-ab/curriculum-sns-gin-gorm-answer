package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	"github.com/zishang520/engine.io/v2/types"
	socketio "github.com/zishang520/socket.io/v2/socket"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

const cookieName = "sns_session"

type Config struct {
	Port        string
	DatabaseURL string
	FrontendURL string
	JWTSecret   string
}

type App struct {
	cfg     Config
	db      *gorm.DB
	io      *socketio.Server
	wsMu    sync.Mutex
	wsRooms map[uint]map[*websocket.Conn]bool
}

type User struct {
	ID            uint   `gorm:"primaryKey"`
	Email         string `gorm:"uniqueIndex;not null"`
	Username      string `gorm:"uniqueIndex;not null"`
	DisplayName   string `gorm:"not null"`
	PasswordHash  string `gorm:"not null"`
	Bio           string
	AvatarURL     string
	EmailVerified bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type EmailVerificationToken struct {
	ID        uint   `gorm:"primaryKey"`
	UserID    uint   `gorm:"not null;index"`
	Token     string `gorm:"uniqueIndex;not null"`
	CreatedAt time.Time
}

type Post struct {
	ID        uint `gorm:"primaryKey"`
	AuthorID  uint `gorm:"not null;index"`
	Author    User
	Content   string `gorm:"not null"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Like struct {
	UserID    uint `gorm:"primaryKey"`
	PostID    uint `gorm:"primaryKey"`
	CreatedAt time.Time
}

type Follow struct {
	FollowerID uint `gorm:"primaryKey"`
	FolloweeID uint `gorm:"primaryKey"`
	CreatedAt  time.Time
}

type Conversation struct {
	ID        uint `gorm:"primaryKey"`
	UserOneID uint `gorm:"not null;uniqueIndex:idx_conversation_pair"`
	UserTwoID uint `gorm:"not null;uniqueIndex:idx_conversation_pair"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Message struct {
	ID             uint `gorm:"primaryKey"`
	ConversationID uint `gorm:"not null;index"`
	SenderID       uint `gorm:"not null;index"`
	Content        string
	CreatedAt      time.Time
}

type registerRequest struct {
	Email       string `json:"email"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Password    string `json:"password"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type postRequest struct {
	Content string `json:"content"`
}

type updateProfileRequest struct {
	DisplayName *string `json:"displayName"`
	Bio         *string `json:"bio"`
	AvatarURL   *string `json:"avatarUrl"`
}

type avatarUploadRequest struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
}

type conversationRequest struct {
	Username string `json:"username"`
}

func main() {
	app, err := newApp(loadConfig())
	if err != nil {
		log.Fatal(err)
	}
	router := app.router()
	log.Fatal(router.Run(":" + app.cfg.Port))
}

func loadConfig() Config {
	return Config{
		Port:        env("PORT", "8000"),
		DatabaseURL: env("DATABASE_URL", "sqlite://sns_gin_gorm.db"),
		FrontendURL: env("FRONTEND_URL", "http://localhost:5173"),
		JWTSecret:   env("JWT_SECRET", "dev-secret"),
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func newApp(cfg Config) (*App, error) {
	db, err := openDB(cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(&User{}, &EmailVerificationToken{}, &Post{}, &Like{}, &Follow{}, &Conversation{}, &Message{}); err != nil {
		return nil, err
	}
	app := &App{cfg: cfg, db: db, wsRooms: map[uint]map[*websocket.Conn]bool{}}
	app.io = app.socketServer()
	return app, nil
}

func openDB(url string) (*gorm.DB, error) {
	if strings.HasPrefix(url, "postgres://") || strings.HasPrefix(url, "postgresql://") {
		return gorm.Open(postgres.Open(url), &gorm.Config{})
	}
	path := strings.TrimPrefix(url, "sqlite://")
	if path == url {
		path = url
	}
	return gorm.Open(sqlite.Open(path), &gorm.Config{})
}

func (a *App) router() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery(), a.cors())
	r.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	r.POST("/auth/register", a.register)
	r.GET("/auth/verify-email", a.verifyEmail)
	r.POST("/auth/login", a.login)
	r.POST("/auth/logout", a.logout)
	r.GET("/auth/me", a.requireUser, a.me)
	r.POST("/posts", a.requireUser, a.createPost)
	r.GET("/posts", a.requireUser, a.listPosts)
	r.GET("/posts/timeline", a.requireUser, a.followingTimeline)
	r.DELETE("/posts/:id", a.requireUser, a.deletePost)
	r.POST("/posts/:id/likes", a.requireUser, a.likePost)
	r.DELETE("/posts/:id/likes", a.requireUser, a.unlikePost)
	r.GET("/users/:username", a.requireUser, a.userProfile)
	r.GET("/users/:username/posts", a.requireUser, a.userPosts)
	r.POST("/users/:username/follow", a.requireUser, a.follow)
	r.DELETE("/users/:username/follow", a.requireUser, a.unfollow)
	r.PATCH("/users/me", a.requireUser, a.updateMe)
	r.POST("/users/me/avatar-upload-url", a.requireUser, a.avatarUploadURL)
	r.PUT("/uploads/avatar/:filename", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	r.GET("/conversations", a.requireUser, a.conversations)
	r.POST("/conversations", a.requireUser, a.createConversation)
	r.GET("/conversations/:id/messages", a.requireUser, a.messages)
	r.POST("/conversations/:id/messages", a.requireUser, a.createMessageHTTP)
	r.GET("/chat", a.websocketChat)
	r.GET("/socket.io/*any", gin.WrapH(a.io.ServeHandler(socketOptions(a.cfg.FrontendURL))))
	r.POST("/socket.io/*any", gin.WrapH(a.io.ServeHandler(socketOptions(a.cfg.FrontendURL))))
	return r
}

func (a *App) cors() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", a.cfg.FrontendURL)
		c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

func socketOptions(frontendURL string) *socketio.ServerOptions {
	opts := socketio.DefaultServerOptions()
	opts.SetCors(&types.Cors{Origin: frontendURL, Credentials: true})
	return opts
}

func (a *App) register(c *gin.Context) {
	var req registerRequest
	if !bind(c, &req) {
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	req.Username = strings.TrimSpace(req.Username)
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	if req.Email == "" || req.Username == "" || req.DisplayName == "" || len(req.Password) < 8 {
		abort(c, http.StatusBadRequest, "入力内容を確認してください", "VALIDATION_ERROR")
		return
	}
	var count int64
	a.db.Model(&User{}).Where("email = ? OR username = ?", req.Email, req.Username).Count(&count)
	if count > 0 {
		abort(c, http.StatusConflict, "メールアドレスまたはユーザー名は既に使われています", "CONFLICT")
		return
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	user := User{Email: req.Email, Username: req.Username, DisplayName: req.DisplayName, PasswordHash: string(hash)}
	if err := a.db.Create(&user).Error; err != nil {
		abort(c, http.StatusInternalServerError, "ユーザー登録に失敗しました", "INTERNAL_ERROR")
		return
	}
	token := fmt.Sprintf("%d-%d", user.ID, time.Now().UnixNano())
	a.db.Create(&EmailVerificationToken{UserID: user.ID, Token: token})
	log.Printf("メール確認URL: %s/#/verify-email?token=%s", a.cfg.FrontendURL, token)
	c.JSON(http.StatusCreated, gin.H{"message": "確認メールを送りました"})
}

func (a *App) verifyEmail(c *gin.Context) {
	token := c.Query("token")
	var record EmailVerificationToken
	if err := a.db.Where("token = ?", token).First(&record).Error; err != nil {
		abort(c, http.StatusNotFound, "確認トークンが見つかりません", "NOT_FOUND")
		return
	}
	a.db.Model(&User{}).Where("id = ?", record.UserID).Update("email_verified", true)
	a.db.Delete(&record)
	c.JSON(http.StatusOK, gin.H{"message": "メールアドレスを確認しました"})
}

func (a *App) login(c *gin.Context) {
	var req loginRequest
	if !bind(c, &req) {
		return
	}
	var user User
	if err := a.db.Where("email = ?", req.Email).First(&user).Error; err != nil {
		abort(c, http.StatusUnauthorized, "メールアドレスまたはパスワードが違います", "UNAUTHENTICATED")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)) != nil {
		abort(c, http.StatusUnauthorized, "メールアドレスまたはパスワードが違います", "UNAUTHENTICATED")
		return
	}
	if !user.EmailVerified {
		abort(c, http.StatusForbidden, "メールアドレスを確認してください", "FORBIDDEN")
		return
	}
	token, err := a.sessionToken(user.ID)
	if err != nil {
		abort(c, http.StatusInternalServerError, "ログインに失敗しました", "INTERNAL_ERROR")
		return
	}
	http.SetCookie(c.Writer, &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 60 * 60 * 24 * 7})
	c.JSON(http.StatusOK, gin.H{"message": "ログインしました"})
}

func (a *App) logout(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	c.Status(http.StatusNoContent)
}

func (a *App) me(c *gin.Context) {
	c.JSON(http.StatusOK, meResponse(current(c)))
}

func (a *App) createPost(c *gin.Context) {
	var req postRequest
	if !bind(c, &req) {
		return
	}
	content := strings.TrimSpace(req.Content)
	if content == "" || len([]rune(content)) > 280 {
		abort(c, http.StatusBadRequest, "投稿内容は1文字以上280文字以内で入力してください", "VALIDATION_ERROR")
		return
	}
	user := current(c)
	post := Post{AuthorID: user.ID, Content: content}
	a.db.Create(&post)
	a.db.Preload("Author").First(&post, post.ID)
	c.JSON(http.StatusCreated, a.postResponse(post, user.ID))
}

func (a *App) listPosts(c *gin.Context) {
	user := current(c)
	var posts []Post
	a.db.Preload("Author").Order("created_at desc, id desc").Find(&posts)
	c.JSON(http.StatusOK, a.postResponses(posts, user.ID))
}

func (a *App) followingTimeline(c *gin.Context) {
	user := current(c)
	ids := []uint{user.ID}
	var follows []Follow
	a.db.Where("follower_id = ?", user.ID).Find(&follows)
	for _, follow := range follows {
		ids = append(ids, follow.FolloweeID)
	}
	var posts []Post
	a.db.Preload("Author").Where("author_id IN ?", ids).Order("created_at desc, id desc").Find(&posts)
	c.JSON(http.StatusOK, a.postResponses(posts, user.ID))
}

func (a *App) deletePost(c *gin.Context) {
	user := current(c)
	var post Post
	if err := a.db.First(&post, c.Param("id")).Error; err != nil {
		abort(c, http.StatusNotFound, "投稿が見つかりません", "NOT_FOUND")
		return
	}
	if post.AuthorID != user.ID {
		abort(c, http.StatusForbidden, "他人の投稿は削除できません", "FORBIDDEN")
		return
	}
	a.db.Delete(&post)
	c.Status(http.StatusNoContent)
}

func (a *App) likePost(c *gin.Context) {
	user := current(c)
	postID, ok := uintParam(c, "id")
	if !ok {
		return
	}
	if !a.postExists(postID) {
		abort(c, http.StatusNotFound, "投稿が見つかりません", "NOT_FOUND")
		return
	}
	a.db.FirstOrCreate(&Like{}, Like{UserID: user.ID, PostID: postID})
	c.Status(http.StatusNoContent)
}

func (a *App) unlikePost(c *gin.Context) {
	user := current(c)
	postID, ok := uintParam(c, "id")
	if !ok {
		return
	}
	a.db.Delete(&Like{}, "user_id = ? AND post_id = ?", user.ID, postID)
	c.Status(http.StatusNoContent)
}

func (a *App) userProfile(c *gin.Context) {
	viewer := current(c)
	user, ok := a.findUserByUsername(c, c.Param("username"))
	if !ok {
		return
	}
	c.JSON(http.StatusOK, a.profileResponse(user, viewer.ID))
}

func (a *App) userPosts(c *gin.Context) {
	viewer := current(c)
	user, ok := a.findUserByUsername(c, c.Param("username"))
	if !ok {
		return
	}
	var posts []Post
	a.db.Preload("Author").Where("author_id = ?", user.ID).Order("created_at desc, id desc").Find(&posts)
	c.JSON(http.StatusOK, a.postResponses(posts, viewer.ID))
}

func (a *App) follow(c *gin.Context) {
	viewer := current(c)
	target, ok := a.findUserByUsername(c, c.Param("username"))
	if !ok {
		return
	}
	if viewer.ID != target.ID {
		a.db.FirstOrCreate(&Follow{}, Follow{FollowerID: viewer.ID, FolloweeID: target.ID})
	}
	c.Status(http.StatusNoContent)
}

func (a *App) unfollow(c *gin.Context) {
	viewer := current(c)
	target, ok := a.findUserByUsername(c, c.Param("username"))
	if !ok {
		return
	}
	a.db.Delete(&Follow{}, "follower_id = ? AND followee_id = ?", viewer.ID, target.ID)
	c.Status(http.StatusNoContent)
}

func (a *App) updateMe(c *gin.Context) {
	user := current(c)
	var req updateProfileRequest
	if !bind(c, &req) {
		return
	}
	if req.DisplayName != nil {
		user.DisplayName = strings.TrimSpace(*req.DisplayName)
	}
	if req.Bio != nil {
		user.Bio = *req.Bio
	}
	if req.AvatarURL != nil {
		user.AvatarURL = *req.AvatarURL
	}
	a.db.Save(&user)
	c.JSON(http.StatusOK, userResponse(user))
}

func (a *App) avatarUploadURL(c *gin.Context) {
	user := current(c)
	var req avatarUploadRequest
	_ = c.ShouldBindJSON(&req)
	name := req.Filename
	if name == "" {
		name = "avatar.png"
	}
	filename := fmt.Sprintf("%d-%d-%s", user.ID, time.Now().UnixNano(), sanitizeFilename(name))
	url := fmt.Sprintf("http://%s/uploads/avatar/%s", c.Request.Host, filename)
	c.JSON(http.StatusOK, gin.H{"uploadUrl": url, "publicUrl": url})
}

func (a *App) conversations(c *gin.Context) {
	user := current(c)
	var conversations []Conversation
	a.db.Where("user_one_id = ? OR user_two_id = ?", user.ID, user.ID).Order("updated_at desc, id desc").Find(&conversations)
	result := make([]gin.H, 0, len(conversations))
	for _, conversation := range conversations {
		result = append(result, a.conversationResponse(conversation, user.ID))
	}
	c.JSON(http.StatusOK, result)
}

func (a *App) createConversation(c *gin.Context) {
	user := current(c)
	var req conversationRequest
	if !bind(c, &req) {
		return
	}
	partner, ok := a.findUserByUsername(c, req.Username)
	if !ok {
		return
	}
	conversation := a.findOrCreateConversation(user.ID, partner.ID)
	c.JSON(http.StatusOK, a.conversationResponse(conversation, user.ID))
}

func (a *App) messages(c *gin.Context) {
	user := current(c)
	conversation, ok := a.conversationForUser(c, user.ID)
	if !ok {
		return
	}
	var messages []Message
	a.db.Where("conversation_id = ?", conversation.ID).Order("created_at asc, id asc").Find(&messages)
	result := make([]gin.H, 0, len(messages))
	for _, message := range messages {
		result = append(result, messageResponse(message))
	}
	c.JSON(http.StatusOK, result)
}

func (a *App) createMessageHTTP(c *gin.Context) {
	user := current(c)
	conversation, ok := a.conversationForUser(c, user.ID)
	if !ok {
		return
	}
	var req postRequest
	if !bind(c, &req) {
		return
	}
	message, ok := a.createMessage(conversation.ID, user.ID, req.Content)
	if !ok {
		abort(c, http.StatusBadRequest, "メッセージを入力してください", "VALIDATION_ERROR")
		return
	}
	c.JSON(http.StatusCreated, messageResponse(message))
}

func (a *App) requireUser(c *gin.Context) {
	user, err := a.userFromRequest(c.Request)
	if err != nil {
		abort(c, http.StatusUnauthorized, "ログインが必要です", "UNAUTHENTICATED")
		c.Abort()
		return
	}
	c.Set("user", user)
	c.Next()
}

func (a *App) userFromRequest(r *http.Request) (User, error) {
	cookie, err := r.Cookie(cookieName)
	if err != nil {
		return User{}, err
	}
	return a.userFromToken(cookie.Value)
}

func (a *App) userFromToken(token string) (User, error) {
	claims := jwt.MapClaims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(token *jwt.Token) (any, error) {
		return []byte(a.cfg.JWTSecret), nil
	})
	if err != nil || !parsed.Valid {
		return User{}, errors.New("invalid token")
	}
	sub, err := strconv.Atoi(fmt.Sprint(claims["sub"]))
	if err != nil {
		return User{}, err
	}
	var user User
	if err := a.db.First(&user, sub).Error; err != nil {
		return User{}, err
	}
	return user, nil
}

func (a *App) sessionToken(userID uint) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": fmt.Sprint(userID),
		"exp": time.Now().Add(7 * 24 * time.Hour).Unix(),
	}).SignedString([]byte(a.cfg.JWTSecret))
}

func (a *App) socketServer() *socketio.Server {
	io := socketio.NewServer(nil, nil)
	chat := io.Of("/chat", nil)
	chat.On("connection", func(args ...any) {
		client := args[0].(*socketio.Socket)
		token := cookieFromHeaders(client.Handshake().Headers, cookieName)
		user, err := a.userFromToken(token)
		if err != nil {
			client.Disconnect(true)
			return
		}
		client.SetData(user.ID)
		client.On("joinConversation", func(values ...any) {
			conversationID := uintFromPayload(values, "conversationId")
			if conversationID == 0 || !a.isConversationParticipant(conversationID, user.ID) {
				return
			}
			client.Join(socketio.Room(roomName(conversationID)))
		})
		client.On("sendMessage", func(values ...any) {
			conversationID := uintFromPayload(values, "conversationId")
			content := stringFromPayload(values, "content")
			if conversationID == 0 || !a.isConversationParticipant(conversationID, user.ID) {
				return
			}
			message, ok := a.createMessage(conversationID, user.ID, content)
			if !ok {
				return
			}
			chat.To(socketio.Room(roomName(conversationID))).Emit("newMessage", messageResponse(message))
		})
	})
	return io
}

type websocketPacket struct {
	Type           string `json:"type"`
	ConversationID uint   `json:"conversationId"`
	Content        string `json:"content"`
}

func (a *App) websocketChat(c *gin.Context) {
	user, err := a.userFromRequest(c.Request)
	if err != nil {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return r.Header.Get("Origin") == a.cfg.FrontendURL
		},
	}
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	joined := map[uint]bool{}
	defer func() {
		a.wsMu.Lock()
		defer a.wsMu.Unlock()
		for conversationID := range joined {
			delete(a.wsRooms[conversationID], conn)
			if len(a.wsRooms[conversationID]) == 0 {
				delete(a.wsRooms, conversationID)
			}
		}
	}()

	for {
		var packet websocketPacket
		if err := conn.ReadJSON(&packet); err != nil {
			return
		}
		switch packet.Type {
		case "joinConversation":
			if packet.ConversationID == 0 || !a.isConversationParticipant(packet.ConversationID, user.ID) {
				continue
			}
			a.wsMu.Lock()
			if a.wsRooms[packet.ConversationID] == nil {
				a.wsRooms[packet.ConversationID] = map[*websocket.Conn]bool{}
			}
			a.wsRooms[packet.ConversationID][conn] = true
			a.wsMu.Unlock()
			joined[packet.ConversationID] = true
		case "sendMessage":
			if packet.ConversationID == 0 || !a.isConversationParticipant(packet.ConversationID, user.ID) {
				continue
			}
			message, ok := a.createMessage(packet.ConversationID, user.ID, packet.Content)
			if !ok {
				continue
			}
			a.broadcastWebsocketMessage(packet.ConversationID, messageResponse(message))
		}
	}
}

func (a *App) broadcastWebsocketMessage(conversationID uint, message gin.H) {
	payload, err := json.Marshal(gin.H{"type": "newMessage", "message": message})
	if err != nil {
		return
	}
	a.wsMu.Lock()
	connections := make([]*websocket.Conn, 0, len(a.wsRooms[conversationID]))
	for conn := range a.wsRooms[conversationID] {
		connections = append(connections, conn)
	}
	a.wsMu.Unlock()

	for _, conn := range connections {
		if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
			a.wsMu.Lock()
			delete(a.wsRooms[conversationID], conn)
			a.wsMu.Unlock()
			_ = conn.Close()
		}
	}
}

func (a *App) createMessage(conversationID, senderID uint, content string) (Message, bool) {
	content = strings.TrimSpace(content)
	if content == "" || len([]rune(content)) > 1000 {
		return Message{}, false
	}
	message := Message{ConversationID: conversationID, SenderID: senderID, Content: content}
	a.db.Create(&message)
	return message, true
}

func (a *App) findOrCreateConversation(aID, bID uint) Conversation {
	ids := []uint{aID, bID}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var conversation Conversation
	if err := a.db.Where("user_one_id = ? AND user_two_id = ?", ids[0], ids[1]).First(&conversation).Error; err == nil {
		return conversation
	}
	conversation = Conversation{UserOneID: ids[0], UserTwoID: ids[1]}
	a.db.Create(&conversation)
	return conversation
}

func (a *App) conversationForUser(c *gin.Context, userID uint) (Conversation, bool) {
	id, ok := uintParam(c, "id")
	if !ok {
		return Conversation{}, false
	}
	var conversation Conversation
	if err := a.db.First(&conversation, id).Error; err != nil || !participant(conversation, userID) {
		abort(c, http.StatusNotFound, "会話が見つかりません", "NOT_FOUND")
		return Conversation{}, false
	}
	return conversation, true
}

func (a *App) isConversationParticipant(conversationID, userID uint) bool {
	var conversation Conversation
	if err := a.db.First(&conversation, conversationID).Error; err != nil {
		return false
	}
	return participant(conversation, userID)
}

func participant(conversation Conversation, userID uint) bool {
	return conversation.UserOneID == userID || conversation.UserTwoID == userID
}

func (a *App) conversationResponse(conversation Conversation, viewerID uint) gin.H {
	partnerID := conversation.UserOneID
	if partnerID == viewerID {
		partnerID = conversation.UserTwoID
	}
	var partner User
	a.db.First(&partner, partnerID)
	var last Message
	var lastValue any
	if err := a.db.Where("conversation_id = ?", conversation.ID).Order("created_at desc, id desc").First(&last).Error; err == nil {
		lastValue = messageResponse(last)
	}
	return gin.H{"id": conversation.ID, "partner": userResponse(partner), "lastMessage": lastValue}
}

func (a *App) postResponses(posts []Post, viewerID uint) []gin.H {
	result := make([]gin.H, 0, len(posts))
	for _, post := range posts {
		result = append(result, a.postResponse(post, viewerID))
	}
	return result
}

func (a *App) postResponse(post Post, viewerID uint) gin.H {
	var likeCount int64
	a.db.Model(&Like{}).Where("post_id = ?", post.ID).Count(&likeCount)
	var liked int64
	a.db.Model(&Like{}).Where("post_id = ? AND user_id = ?", post.ID, viewerID).Count(&liked)
	return gin.H{"id": post.ID, "content": post.Content, "createdAt": post.CreatedAt, "author": userResponse(post.Author), "likeCount": likeCount, "likedByMe": liked > 0}
}

func (a *App) profileResponse(user User, viewerID uint) gin.H {
	var followers int64
	a.db.Model(&Follow{}).Where("followee_id = ?", user.ID).Count(&followers)
	var following int64
	a.db.Model(&Follow{}).Where("follower_id = ?", user.ID).Count(&following)
	var isFollowing int64
	a.db.Model(&Follow{}).Where("follower_id = ? AND followee_id = ?", viewerID, user.ID).Count(&isFollowing)
	return gin.H{"id": user.ID, "username": user.Username, "displayName": user.DisplayName, "bio": user.Bio, "avatarUrl": nullableString(user.AvatarURL), "followersCount": followers, "followingCount": following, "isFollowing": isFollowing > 0}
}

func userResponse(user User) gin.H {
	return gin.H{"id": user.ID, "username": user.Username, "displayName": user.DisplayName, "bio": user.Bio, "avatarUrl": nullableString(user.AvatarURL)}
}

func meResponse(user User) gin.H {
	return gin.H{"id": user.ID, "username": user.Username, "displayName": user.DisplayName, "bio": user.Bio, "avatarUrl": nullableString(user.AvatarURL), "email": user.Email}
}

func messageResponse(message Message) gin.H {
	return gin.H{"id": message.ID, "conversationId": message.ConversationID, "senderId": message.SenderID, "content": message.Content, "createdAt": message.CreatedAt}
}

func (a *App) findUserByUsername(c *gin.Context, username string) (User, bool) {
	var user User
	if err := a.db.Where("username = ?", username).First(&user).Error; err != nil {
		abort(c, http.StatusNotFound, "ユーザーが見つかりません", "NOT_FOUND")
		return User{}, false
	}
	return user, true
}

func (a *App) postExists(id uint) bool {
	var count int64
	a.db.Model(&Post{}).Where("id = ?", id).Count(&count)
	return count > 0
}

func current(c *gin.Context) User {
	return c.MustGet("user").(User)
}

func bind(c *gin.Context, value any) bool {
	if err := c.ShouldBindJSON(value); err != nil {
		abort(c, http.StatusBadRequest, "入力内容を確認してください", "VALIDATION_ERROR")
		return false
	}
	return true
}

func abort(c *gin.Context, status int, message, code string) {
	c.AbortWithStatusJSON(status, gin.H{"message": message, "code": code})
}

func uintParam(c *gin.Context, name string) (uint, bool) {
	value, err := strconv.ParseUint(c.Param(name), 10, 64)
	if err != nil {
		abort(c, http.StatusBadRequest, "IDが不正です", "VALIDATION_ERROR")
		return 0, false
	}
	return uint(value), true
}

func uintFromPayload(values []any, key string) uint {
	if len(values) == 0 {
		return 0
	}
	payload, ok := values[0].(map[string]any)
	if !ok {
		return 0
	}
	switch v := payload[key].(type) {
	case float64:
		return uint(v)
	case int:
		return uint(v)
	case uint:
		return v
	case string:
		n, _ := strconv.ParseUint(v, 10, 64)
		return uint(n)
	default:
		return 0
	}
}

func stringFromPayload(values []any, key string) string {
	if len(values) == 0 {
		return ""
	}
	payload, ok := values[0].(map[string]any)
	if !ok {
		return ""
	}
	return fmt.Sprint(payload[key])
}

func cookieFromHeaders(headers map[string][]string, name string) string {
	for key, values := range headers {
		if strings.EqualFold(key, "cookie") {
			for _, header := range values {
				for _, part := range strings.Split(header, ";") {
					part = strings.TrimSpace(part)
					prefix := name + "="
					if strings.HasPrefix(part, prefix) {
						return strings.TrimPrefix(part, prefix)
					}
				}
			}
		}
	}
	return ""
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, "\\", "-")
	return name
}

func roomName(conversationID uint) string {
	return fmt.Sprintf("conversation:%d", conversationID)
}
