all:
	cd main && qtc
	mv main/*.go .
	go build -o app *.go

setup:
	go get -u .
