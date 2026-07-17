set CGO_ENABLED=0
set GOOS=windows
set GOARCH=amd64

go build -trimpath -ldflags="-s -w" -o anicli.exe