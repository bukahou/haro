// Package appapi 是 App 接口的服务端代码：appapi.gen.go 由 oapi-codegen 从 api/openapi.yaml 生成（⛔ 不手改），
// 实现放在别的包里（照契约的 StrictServerInterface）。契约改了就重新生成。
package appapi

//go:generate go tool oapi-codegen -config ../../api/oapi-codegen.yaml ../../api/openapi.yaml
