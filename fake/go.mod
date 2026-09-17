module github.com/automagicops/haliphron/fake

go 1.25.0

require github.com/automagicops/haliphron/api v0.0.0

require (
	k8s.io/api v0.34.0 // indirect
	k8s.io/apimachinery v0.34.1 // indirect
)

replace github.com/automagicops/haliphron/api => ../api
