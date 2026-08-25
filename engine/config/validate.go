package config

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Validate checks whether a configuration contains the minimum
// information required to be processed by a protocol engine.
//
// Validate does not perform network access and does not determine
// whether the remote server is currently working.
func (c *Config) Validate() error {
	c.Normalize()

	if c.Type == "" || c.Type == TypeUnknown {
		return fmt.Errorf("unsupported or missing protocol type")
	}

	if strings.TrimSpace(c.Address) == "" {
		return fmt.Errorf("server address is required")
	}

	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("invalid port: %d", c.Port)
	}

	switch c.Type {
	case TypeVLESS:
		return validateVLESS(c)

	case TypeVMess:
		return validateVMess(c)

	case TypeTrojan:
		return validateTrojan(c)

	case TypeShadowsocks:
		return validateShadowsocks(c)

	case TypeHysteria, TypeHysteria2:
		return validateHysteria(c)

	case TypeTUIC:
		return validateTUIC(c)

	case TypeWireGuard:
		return validateWireGuard(c)

	case TypeSOCKS, TypeHTTP:
		return validateProxy(c)

	default:
		return fmt.Errorf("unsupported protocol: %s", c.Type)
	}
}

func validateVLESS(c *Config) error {
	if strings.TrimSpace(c.UUID) == "" {
		return fmt.Errorf("VLESS UUID is required")
	}

	return nil
}

func validateVMess(c *Config) error {
	if strings.TrimSpace(c.UUID) == "" {
		return fmt.Errorf("VMess UUID is required")
	}

	return nil
}

func validateTrojan(c *Config) error {
	if strings.TrimSpace(c.Password) == "" {
		return fmt.Errorf("Trojan password is required")
	}

	return nil
}

func validateShadowsocks(c *Config) error {
	if strings.TrimSpace(c.Method) == "" {
		return fmt.Errorf("Shadowsocks method is required")
	}

	if strings.TrimSpace(c.Password) == "" {
		return fmt.Errorf("Shadowsocks password is required")
	}

	return nil
}

func validateHysteria(c *Config) error {
	if strings.TrimSpace(c.Password) == "" &&
		strings.TrimSpace(c.UUID) == "" {
		return fmt.Errorf("Hysteria authentication is required")
	}

	return nil
}

func validateTUIC(c *Config) error {
	if strings.TrimSpace(c.UUID) == "" {
		return fmt.Errorf("TUIC UUID is required")
	}

	if strings.TrimSpace(c.Password) == "" {
		return fmt.Errorf("TUIC password is required")
	}

	return nil
}

func validateWireGuard(c *Config) error {
	if strings.TrimSpace(c.PublicKey) == "" {
		return fmt.Errorf("WireGuard public key is required")
	}

	return nil
}

func validateProxy(c *Config) error {
	return nil
}

// Endpoint returns the normalized host:port representation of a config.
func (c *Config) Endpoint() string {
	return net.JoinHostPort(c.Address, strconv.Itoa(c.Port))
}
