module github.com/mac-lucky/pushward-integrations/bambulab

go 1.25.7

require (
	github.com/eclipse/paho.mqtt.golang v1.5.0
	github.com/mac-lucky/pushward-integrations/shared v0.0.0
)

require (
	github.com/gorilla/websocket v1.5.3 // indirect
	golang.org/x/net v0.27.0 // indirect
	golang.org/x/sync v0.7.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/mac-lucky/pushward-integrations/shared => ../shared
