module github.com/criyle/go-judge

go 1.16

require (
	cloud.google.com/go v0.81.0 // indirect
	github.com/creack/pty v1.1.11
	github.com/criyle/go-sandbox v0.7.3
	github.com/elastic/go-seccomp-bpf v1.1.0
	github.com/elastic/go-ucfg v0.8.3
	github.com/fatih/camelcase v1.0.0 // indirect
	github.com/fatih/structs v1.1.0 // indirect
	github.com/gin-contrib/pprof v1.3.0
	github.com/gin-contrib/zap v0.0.1
	github.com/gin-gonic/gin v1.7.1
	github.com/go-playground/validator/v10 v10.5.0 // indirect
	github.com/golang/protobuf v1.5.2
	github.com/gorilla/websocket v1.4.2
	github.com/grpc-ecosystem/go-grpc-middleware v1.3.0
	github.com/grpc-ecosystem/go-grpc-prometheus v1.2.0
	github.com/koding/multiconfig v0.0.0-20171124222453-69c27309b2d7
	github.com/kr/text v0.2.0 // indirect
	github.com/leodido/go-urn v1.2.1 // indirect
	github.com/prometheus/client_golang v1.10.0
	github.com/prometheus/common v0.21.0 // indirect
	github.com/sirupsen/logrus v1.8.1 // indirect
	github.com/ugorji/go v1.2.5 // indirect
	github.com/zsais/go-gin-prometheus v0.1.0
	go.uber.org/multierr v1.6.0 // indirect
	go.uber.org/zap v1.16.0
	golang.org/x/crypto v0.0.0-20210421170649-83a5a9bb288b
	golang.org/x/mod v0.4.2 // indirect
	golang.org/x/net v0.0.0-20210423184538-5f58ad60dda6
	golang.org/x/oauth2 v0.0.0-20210413134643-5e61552d6c78 // indirect
	golang.org/x/sync v0.0.0-20210220032951-036812b2e83c
	golang.org/x/sys v0.0.0-20210423185535-09eb48e85fd7
	golang.org/x/term v0.0.0-20210422114643-f5beecf764ed // indirect
	google.golang.org/genproto v0.0.0-20210423144448-3a41ef94ed2b // indirect
	google.golang.org/grpc v1.37.0
	google.golang.org/protobuf v1.26.0
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
	gopkg.in/yaml.v2 v2.4.0
	honnef.co/go/tools v0.1.3 // indirect
)

retract (
	// File descripter leak when multiple container fork at the same time
	[v0.9.5, v1.1.4]
	// Old version, don't use
	[v0.0.1, v0.9.4]
)
