module github.com/sirkostya009/ggen/integrationtests

go 1.27

replace github.com/sirkostya009/ggen => ../

require (
	github.com/gofrs/uuid/v5 v5.4.0
	github.com/google/uuid v1.6.0
	github.com/sirkostya009/ggen v0.0.0
	github.com/sirkostya009/ggen/gen v0.0.0
)

require (
	golang.org/x/mod v0.37.0 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/tools v0.46.0 // indirect
)

replace github.com/sirkostya009/ggen/gen => ../gen
