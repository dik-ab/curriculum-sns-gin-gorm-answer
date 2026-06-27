package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestPostLikeFollowAndConversationFlow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app, err := newApp(Config{
		Port:        "0",
		DatabaseURL: "file::memory:?cache=shared",
		FrontendURL: "http://localhost:5173",
		JWTSecret:   "test-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	router := app.router()

	aliceCookie := registerVerifyLogin(t, app, router, "alice@example.com", "alice")
	bobCookie := registerVerifyLogin(t, app, router, "bob@example.com", "bob")

	post := request(t, router, http.MethodPost, "/posts", `{"content":"Goからこんにちは"}`, bobCookie)
	if post.Code != http.StatusCreated {
		t.Fatalf("post status = %d body=%s", post.Code, post.Body.String())
	}

	timeline := request(t, router, http.MethodGet, "/posts", "", aliceCookie)
	if !strings.Contains(timeline.Body.String(), "Goからこんにちは") {
		t.Fatalf("timeline does not contain post: %s", timeline.Body.String())
	}

	like := request(t, router, http.MethodPost, "/posts/1/likes", "", aliceCookie)
	if like.Code != http.StatusNoContent {
		t.Fatalf("like status = %d", like.Code)
	}
	liked := request(t, router, http.MethodGet, "/posts", "", aliceCookie)
	if !strings.Contains(liked.Body.String(), `"likeCount":1`) || !strings.Contains(liked.Body.String(), `"likedByMe":true`) {
		t.Fatalf("like fields are wrong: %s", liked.Body.String())
	}

	follow := request(t, router, http.MethodPost, "/users/bob/follow", "", aliceCookie)
	if follow.Code != http.StatusNoContent {
		t.Fatalf("follow status = %d", follow.Code)
	}
	profile := request(t, router, http.MethodGet, "/users/bob", "", aliceCookie)
	if !strings.Contains(profile.Body.String(), `"isFollowing":true`) {
		t.Fatalf("profile follow field is wrong: %s", profile.Body.String())
	}

	conversation := request(t, router, http.MethodPost, "/conversations", `{"username":"bob"}`, aliceCookie)
	if conversation.Code != http.StatusOK || !strings.Contains(conversation.Body.String(), `"username":"bob"`) {
		t.Fatalf("conversation failed: %d %s", conversation.Code, conversation.Body.String())
	}
}

func TestLogoutClearsCookie(t *testing.T) {
	gin.SetMode(gin.TestMode)
	app, err := newApp(Config{
		Port:        "0",
		DatabaseURL: "file::memory:?cache=shared",
		FrontendURL: "http://localhost:5173",
		JWTSecret:   "test-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	router := app.router()
	cookie := registerVerifyLogin(t, app, router, "logout@example.com", "logout")
	res := request(t, router, http.MethodPost, "/auth/logout", "", cookie)
	if res.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d", res.Code)
	}
	if setCookie := res.Header().Get("Set-Cookie"); !strings.Contains(setCookie, "sns_session=") || !strings.Contains(setCookie, "Max-Age=0") {
		t.Fatalf("logout Set-Cookie is wrong: %s", setCookie)
	}
}

func registerVerifyLogin(t *testing.T, app *App, router *gin.Engine, email, username string) string {
	t.Helper()
	body := `{"email":"` + email + `","username":"` + username + `","displayName":"` + username + `","password":"password123"}`
	if res := request(t, router, http.MethodPost, "/auth/register", body, ""); res.Code != http.StatusCreated {
		t.Fatalf("register status = %d body=%s", res.Code, res.Body.String())
	}
	var token EmailVerificationToken
	if err := app.db.Order("id desc").First(&token).Error; err != nil {
		t.Fatal(err)
	}
	if res := request(t, router, http.MethodGet, "/auth/verify-email?token="+token.Token, "", ""); res.Code != http.StatusOK {
		t.Fatalf("verify status = %d", res.Code)
	}
	login := request(t, router, http.MethodPost, "/auth/login", `{"email":"`+email+`","password":"password123"}`, "")
	if login.Code != http.StatusOK {
		t.Fatalf("login status = %d body=%s", login.Code, login.Body.String())
	}
	return login.Header().Get("Set-Cookie")
}

func request(t *testing.T, router *gin.Engine, method, path, body, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)
	return res
}
