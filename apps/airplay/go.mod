module capsulad/apps/airplay

go 1.22.5

require (
	capsulad/sdk v0.0.0
	golang.org/x/crypto v0.27.0
)

require golang.org/x/sys v0.25.0 // indirect

replace capsulad/sdk => ../../sdk/go
