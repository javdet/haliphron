module github.com/automagicops/haliphron/backend

go 1.25.0

require (
	github.com/automagicops/haliphron/api v0.0.0
	github.com/automagicops/haliphron/db v0.0.0
	github.com/jackc/pgx/v5 v5.7.6
)

replace github.com/automagicops/haliphron/api => ../api

replace github.com/automagicops/haliphron/db => ../db
