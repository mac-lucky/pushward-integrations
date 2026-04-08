module github.com/mac-lucky/pushward-integrations/bambulab

go 1.26.2

require (
	github.com/eclipse/paho.mqtt.golang v1.5.1
	github.com/mac-lucky/pushward-integrations/shared v0.0.0
)

require (
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/kr/text v0.2.0 // indirect
	golang.org/x/net v0.51.0 // indirect
	golang.org/x/sync v0.19.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/mac-lucky/pushward-integrations/shared => ../shared
