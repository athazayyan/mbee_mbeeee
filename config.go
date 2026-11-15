package main

import (
	"errors"
	"net/url"
)

func appOrigin(value string) (string, error) {
	origin, err := url.Parse(value)
	if err != nil || origin.Hostname() == "" || origin.User != nil ||
		value != origin.Scheme+"://"+origin.Host {
		return "", errors.New("APP_ORIGIN must be an origin such as https://app.example.com")
	}
	loopback := origin.Hostname() == "localhost" || origin.Hostname() == "127.0.0.1" || origin.Hostname() == "::1"
	if origin.Scheme != "https" && !(origin.Scheme == "http" && loopback) {
		return "", errors.New("APP_ORIGIN must use HTTPS outside local development")
	}
	return value, nil
}
