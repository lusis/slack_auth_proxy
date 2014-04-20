package main

import (
	"log"
	"github.com/tappleby/slack-auth-proxy/slack"
	"net/http"
	"fmt"
	"net/http/httputil"
	"strings"
	"github.com/gorilla/securecookie"
	"time"
	"html/template"
	"encoding/base64"
)

const (
	signInPath = "/oauth2/sign_in"
	oauthStartPath = "/oauth2/start"
	oauthCallbackPath = "/oauth2/callback"
	staticDir = "/_slackproxy"
)

var (
	oauthTemplates = template.Must(template.ParseGlob("templates/*.html"))
)

type OAuthServer struct {
	CookieKey string
	Validator func(*slack.Auth, *UpstreamConfiguration) bool

	slackOauth *slack.OAuthClient
	serveMux	*http.ServeMux
	staticHandler http.Handler

	secureCookie *securecookie.SecureCookie
	upstreamsConfig UpstreamConfigurationMap

	config *Configuration
}

func NewOauthServer(slackOauth *slack.OAuthClient, config *Configuration) *OAuthServer {
	serveMux := http.NewServeMux()
	upstreamsPathMap := make(UpstreamConfigurationMap)

	for _, upstream := range config.Upstreams {
		u := upstream.HostURL
		path := u.Path
		u.Path = ""

		if path == "" {
			path = "/"
		}

		log.Printf("mapping %s => %s", path, u)
		serveMux.Handle(path, httputil.NewSingleHostReverseProxy(u))

		upstreamsPathMap[path] = upstream
	}

	decode64 := func(name, s string) []byte {
		dec := base64.StdEncoding
		bs, err := dec.DecodeString(s)
		if err != nil {
			log.Fatalf("Could not decode %s key: %s", name, err)
		}
		return bs
	}
	hashKey := decode64("cookie_hash_key", config.CookieHashKey)
	blockKey := decode64("cookie_block_key", config.CookieBlockKey)

	secureCookie := securecookie.New(hashKey, blockKey)


	return &OAuthServer{
		CookieKey: "_slackauthproxy",
		Validator: NewValidator(),
		serveMux: serveMux,
		slackOauth: slackOauth,
		upstreamsConfig: upstreamsPathMap,
		secureCookie: secureCookie,
		staticHandler: http.FileServer(http.Dir("static")),
		config: config,
	}
}

func (s *OAuthServer) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	var ok bool

	// check if this is a redirect back at the end of oauth
	remoteIP := req.Header.Get("X-Real-IP")
	if remoteIP == "" {
		remoteIP = req.RemoteAddr
	}
	log.Printf("%s %s %s", remoteIP, req.Method, req.URL.Path)

	reqPath := req.URL.Path

	if reqPath == signInPath {
		s.handleSignIn(rw, req)
		return
	} else if reqPath == oauthStartPath {
		s.handleOAuthStart(rw, req)
		return
	} else if (reqPath == oauthCallbackPath) {
		s.handleOAuthCallback(rw, req)
		return
	} else if (strings.HasPrefix(reqPath, staticDir)) {
		req.URL.Path = strings.Replace(reqPath, staticDir, "", 1)
		s.staticHandler.ServeHTTP(rw, req);
		return;
	}

	handler, pattern := s.serveMux.Handler(req)
 	upstreamConfig := s.upstreamsConfig[pattern]

	if upstreamConfig == nil {
		pattern = strings.TrimPrefix(pattern, "/")
		upstreamConfig = s.upstreamsConfig[pattern]
	}

	if upstreamConfig == nil {
		http.NotFound(rw, req)
		return
	}

	if !ok {
		cookie, _ := req.Cookie(s.CookieKey)

		if cookie != nil {
			auth := new(slack.Auth)
			s.secureCookie.Decode(s.CookieKey, cookie.Value, &auth);

			ok = s.Validator(auth, upstreamConfig)
		}
	}

	if !ok {
		log.Printf("invalid cookie")
		s.handleSignIn(rw, req)
		return
	}

	handler.ServeHTTP(rw, req)
}

func (s *OAuthServer) GetRedirect(req *http.Request) (string, error) {
	err := req.ParseForm()

	if err != nil {
		return "", err
	}

	redirect := req.FormValue("rd")

	if redirect == "" || redirect == signInPath {
		redirect = "/"
	}

	return redirect, err
}

func (s *OAuthServer) handleSignIn(rw http.ResponseWriter, req *http.Request) {
	s.ClearCookie(rw, req)

	t := struct {
		Title string
		Redirect string
	}{
		Title: "Sign in",
		Redirect: req.URL.RequestURI(),
	}

	s.renderTemplate(rw, "sign_in", t)
}

func (s *OAuthServer) handleOAuthStart(rw http.ResponseWriter, req *http.Request) {
	redirect, err := s.GetRedirect(req)
	if err != nil {
		s.ErrorPage(rw, 500, "Internal Error", err.Error())
		return
	}

	http.Redirect(rw, req, s.slackOauth.LoginUrl(redirect).String(), 302)
}

func (s *OAuthServer) handleOAuthCallback(rw http.ResponseWriter, req *http.Request) {
	// finish the oauth cycle
	err := req.ParseForm()
	if err != nil {
		s.ErrorPage(rw, 500, "Internal Error", err.Error())
		return
	}
	errorString := req.Form.Get("error")
	if errorString != "" {
		s.ErrorPage(rw, 403, "Permission Denied", errorString)
		return
	}

	access, err := s.slackOauth.RedeemCode(req.Form.Get("code"))

	if err != nil {
		log.Printf("error redeeming code %s", err.Error())
		s.ErrorPage(rw, 500, "Internal Error", err.Error())
		return
	}

	cl := slack.NewClient(access.Token)
	auth, err := cl.Auth.Test()

	if err != nil {
		log.Printf("error redeeming code %s", err.Error())
		s.ErrorPage(rw, 500, "Internal Error", err.Error())
		return
	}

	encoded, err := s.secureCookie.Encode(s.CookieKey, auth)

	if err != nil {
		log.Printf("Error encoding cookie %s", err.Error())
		s.ErrorPage(rw, 500, "Internal Error", "Error encoding auth cookie")
	}

	redirect := req.Form.Get("state")
	if redirect == "" {
		redirect = "/"
	}

	log.Printf("authenticating %s completed", auth.Username)

	s.SetCookie(rw, req, encoded)
	http.Redirect(rw, req, redirect, 302)
}

func (s *OAuthServer) ErrorPage(rw http.ResponseWriter, code int, title string, message string) {
	log.Printf("ErrorPage %d %s %s", code, title, message)
	rw.WriteHeader(code)
	t := struct {
			Title   string
			Message string
		}{
		Title:   fmt.Sprintf("%d %s", code, title),
		Message: message,
	}

	s.renderTemplate(rw, "error", t)
}

func (s *OAuthServer) SetCookie(rw http.ResponseWriter, req *http.Request, val string) {
	cookie := &http.Cookie{
		Name:     s.CookieKey,
		Value:   val,
		Path:     "/",
		Domain:   s.getCookieDomain(req),
		Expires:  time.Now().Add(time.Duration(168) * time.Hour), // 7 days
		HttpOnly: true,
		// Secure: req. ... ? set if X-Scheme: https ?
	}

	http.SetCookie(rw, cookie)
}

func (s *OAuthServer) ClearCookie(rw http.ResponseWriter, req *http.Request) {
	cookie := &http.Cookie{
		Name:     s.CookieKey,
		Value:    "",
		Path:     "/",
		Domain:   s.getCookieDomain(req),
		Expires:  time.Now().Add(time.Duration(1) * time.Hour * -1),
		HttpOnly: true,
	}
	http.SetCookie(rw, cookie)
}

func (s *OAuthServer) renderTemplate(rw http.ResponseWriter, tmpl string, data interface {}) {
	err := oauthTemplates.ExecuteTemplate(rw, tmpl+".html", data)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusInternalServerError)
	}
}

func (s *OAuthServer) getCookieDomain(req *http.Request) string {
	domain := strings.Split(req.Host, ":")[0]
	if s.config.CookieDomain != "" && strings.HasSuffix(domain, s.config.CookieDomain) {
		domain = s.config.CookieDomain
	}

	return domain
}
