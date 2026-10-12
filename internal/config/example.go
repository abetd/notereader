package config

import _ "embed"

// Example は config.example.yaml の中身。notereader config init が雛形として書き出す。
//
//go:embed config.example.yaml
var Example []byte
