module github.com/mac-lucky/pushward-integrations/unraid

go 1.25.7

require github.com/mac-lucky/pushward-integrations/shared v0.0.0

require github.com/coder/websocket v1.8.14

require gopkg.in/yaml.v3 v3.0.1 // indirect

replace github.com/mac-lucky/pushward-integrations/shared => ../shared
