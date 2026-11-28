package appserver

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/rs/cors"
)

// crossOriginMaxAge 是浏览器记住预检结果多久（秒）。
const crossOriginMaxAge = 600

// crossOrigin 让网页版（另一个域名）能从浏览器调接口：只放行 webURL 这一个来源；不带 cookie（令牌在 Authorization 里）。
// 允许的方法从契约里取（一处定义）。没有网页版时原样返回。
func crossOrigin(spec *openapi3.T, webURL string, next http.Handler) (http.Handler, error) {
	if webURL == "" {
		return next, nil
	}
	parsed, err := url.Parse(webURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("网页版的地址 %q 不是 http(s)://主机[:端口]", webURL)
	}
	origin := (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host}).String()
	return cors.New(cors.Options{
		AllowedOrigins: []string{origin},
		AllowedMethods: contractMethods(spec),
		AllowedHeaders: []string{"Authorization", "Content-Type", "Accept-Language"},
		ExposedHeaders: []string{"Retry-After"},
		MaxAge:         crossOriginMaxAge,
	}).Handler(next), nil
}

// contractMethods 是契约里用到的 HTTP 方法。
func contractMethods(spec *openapi3.T) []string {
	methods := map[string]bool{}
	for _, item := range spec.Paths.Map() {
		for method := range item.Operations() {
			methods[method] = true
		}
	}
	return slices.Sorted(maps.Keys(methods))
}
