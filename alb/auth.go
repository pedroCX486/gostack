package alb

import (
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	mrand "math/rand"
	"net/http"
	"regexp"
	"strings"
	"text/template"
	"time"

	cache "github.com/Code-Hex/go-generics-cache"

	"github.com/golang-jwt/jwt/v4"
	"github.com/rs/zerolog/log"
)

// straight from chatgpt
var loginForm = `<!DOCTYPE html>
<html>
  <head>
    <meta charset="utf-8">
    <title>Login</title>
  </head>
  <body>
    <form action="/submit" method="POST">
      <label for="sub">SUB:</label>
      <textarea id="sub" name="sub">user@test.io</textarea>
	  <label for="introspection">INTROSPECTION:</label>
      <textarea id="introspection" name="introspection">{"active": true,"scope": "openid"}</textarea>
	  <label for="userinfo">USERINFO:</label>
      <textarea id="userinfo" name="userinfo">{}</textarea>
      <button type="submit">Submit</button>
    </form>
  </body>
</html>
`

func (alb *ALB) authLoginHandler() http.HandlerFunc {
	tmpl := template.Must(template.New("login").Parse(loginForm))
	return func(w http.ResponseWriter, r *http.Request) {
		if err := tmpl.Execute(w, ""); err != nil {
			log.Error().Err(err).Msg("unable to render auth login page")
		}
	}
}

func (alb *ALB) OidcHandler(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, _ := store.Get(r, "gostack")

		// Check if user is authenticated
		vals := sess.Values
		if _, ok := vals["authenticated"].(bool); !ok {
			http.Redirect(w, r, "http://auth.127.0.0.1.nip.io:8080/login", http.StatusFound)
			return
		}

		token := sess.Values["token"].(string)
		data := alb.mockData[token]

		r.Header.Set("x-amzn-oidc-accesstoken", token)
		r.Header.Set("x-amzn-oidc-data", data.OidcData)
		next.ServeHTTP(w, r)
	}
}

func (alb *ALB) signJwt(header, claims map[string]any) (string, error) {
	headerBytes, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	hdr := base64.URLEncoding.EncodeToString(headerBytes)
	claimsBytes, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	clm := base64.URLEncoding.EncodeToString(claimsBytes)
	toSign := strings.Join([]string{hdr, clm}, ".")
	hasher := jwt.SigningMethodES256.Hash.New()
	hasher.Write([]byte(toSign))
	r, s, err := ecdsa.Sign(rand.Reader, alb.signer, hasher.Sum(nil))
	if err != nil {
		return "", err
	}
	curveBits := alb.signer.Curve.Params().BitSize

	keyBytes := curveBits / 8
	if curveBits%8 > 0 {
		keyBytes += 1
	}

	// We serialize the outputs (r and s) into big-endian byte arrays
	// padded with zeros on the left to make sure the sizes work out.
	// Output must be 2*keyBytes long.
	out := make([]byte, 2*keyBytes)
	r.FillBytes(out[0:keyBytes]) // r is assigned to the first half of output.
	s.FillBytes(out[keyBytes:])  // s is assigned to the second half of output.

	return strings.Join([]string{hdr, clm, base64.URLEncoding.EncodeToString(out)}, "."), nil
}

const alphaNumeric = "1234567890abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

func createToken(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alphaNumeric[mrand.Intn(len(alphaNumeric))] //#nosec
	}
	return string(b)
}

func (alb *ALB) authSubmitHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		defer r.Body.Close()

		if !r.Form.Has("sub") {
			log.Error().Msg("the sub field is required to login")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		sub := r.Form.Get("sub")

		code := createToken(32)
		token := createToken(32)
		alb.codeExchange.Set(code, token, cache.WithExpiration(1*time.Minute))

		intro := make(map[string]any)
		_ = json.Unmarshal([]byte(r.Form.Get("introspection")), &intro)
		intro["sub"] = sub
		introResp, _ := json.Marshal(intro)

		claims := make(map[string]any)
		_ = json.Unmarshal([]byte(r.Form.Get("userinfo")), &claims)
		claims["sub"] = sub
		oidcData, _ := alb.signJwt(oidcHeader(), claims)
		alb.mockData[token] = mockdata{
			Sub:           sub,
			Userinfo:      r.Form.Get("userinfo"),
			Introspection: string(introResp),
			OidcData:      oidcData,
		}

		http.Redirect(w, r, fmt.Sprintf("http://alb.127.0.0.1.nip.io:8080/oauth2/idpresponse?code=%s", code), http.StatusFound)
	}
}

func oidcHeader() map[string]any {
	return map[string]interface{}{
		"alg":    "ES256",
		"client": "some-oidc-client",
		"exp":    float64(time.Now().Add(5 * time.Minute).Unix()),
		"iss":    "http://fake.alb.io",
		"kid":    "fakekey",
		"signer": "arn:aws:elasticloadbalancing:eu-west-2:12345678901:loadbalancer/app/demo/d3e0e00f95dd5ef4",
		"typ":    "JWT",
	}
}

func (alb *ALB) idpResponse() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		token, ok := alb.codeExchange.Get(code)
		if !ok {
			log.Error().Msg("unable to find code to exchange, redirecting to login")
			http.Redirect(w, r, "http://auth.127.0.0.1.nip.io:8080/login", http.StatusFound)
			return
		}
		sess, _ := store.Get(r, "gostack")
		sess.Values["authenticated"] = true
		sess.Values["sub"] = alb.mockData[token].Sub
		sess.Values["token"] = token

		if err := sess.Save(r, w); err != nil {
			log.Error().Err(err).Msg("unable to save session for idp response")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "http://alb.127.0.0.1.nip.io:8080/", http.StatusFound)
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Content-Type", "text/html")
	}
}

func (alb *ALB) endSession() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params := r.URL.Query()
		postLogoutRedirectURI := params.Get("post_logout_redirect_uri")
		if postLogoutRedirectURI != "" {
			http.Redirect(w, r, postLogoutRedirectURI, http.StatusFound)
			return
		} else {
			log.Error().Msg("no 'post_logout_redirect_uri' query param provided for the end session endpoint")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}
}

func (alb *ALB) userinfo() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token, err := getTokenFromHeader(r.Header); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
		} else {
			if data, ok := alb.mockData[token]; ok {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(data.Userinfo))
			} else {
				log.Error().Str("token", token).Msg("the token is not in the mock data store")
				w.WriteHeader(http.StatusBadRequest)
			}
		}
	}
}

func getTokenFromHeader(headers map[string][]string) (string, error) {
	auth := headers["Authorization"]
	if len(auth) == 0 {
		return "", fmt.Errorf("no authorization header provided")
	}
	if auth[0] == "" {
		auth = headers["authorization"]
	}
	rx := regexp.MustCompile(`^[bB]earer (.*)$`)
	token := rx.FindStringSubmatch(auth[0])
	if len(token) != 2 {
		return "", fmt.Errorf("unable to extract bearer token from header")
	}
	return token[1], nil
}

func (alb *ALB) introspection() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// TODO it should be authenticated
		if token := r.Form.Get("token"); token != "" {
			if data, ok := alb.mockData[token]; ok {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(data.Introspection))
			} else {
				log.Error().Str("token", token).Msg("the token is not in the mock data store")
				w.WriteHeader(http.StatusBadRequest)
			}
		} else {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}
}
