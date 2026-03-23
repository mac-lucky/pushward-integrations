module github.com/mac-lucky/pushward-integrations/github

go 1.25.8

require github.com/mac-lucky/pushward-integrations/shared v0.0.0

require (
	github.com/kr/text v0.2.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace github.com/mac-lucky/pushward-integrations/shared => ../shared
