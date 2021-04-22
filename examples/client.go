package main

import (
	"context"
	"encoding/xml"
	"log"

	"github.com/Bl4cksunLtd/soap"
)

// FooRequest a simple request
type FooRequest struct {
	XMLName xml.Name `xml:"fooRequest"`
	Foo     string
}

// FooResponse a simple response
type FooResponse struct {
	Bar string
}

func main() {
	client := soap.NewClient("http://127.0.0.1:8080/", nil)
	client.Log = log.Println // verbose
	response := &FooResponse{}
	httpResponse, err := client.Call(context.Background(), "operationFoo", &FooRequest{Foo: "hello i am foo"}, response)
	if err != nil {
		panic(err)
	}
	log.Println(response.Bar, httpResponse.Status)
}
