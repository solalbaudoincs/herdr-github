package main

import (
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"time"
)

// returnEnv is how the popup is told where it was opened from: another
// plugin's popup (herdr-recap's list of agents, say), as JSON, to open again
// when this one's closed. herdr shows one popup at a time, so going from one
// plugin's to another's means closing the first.
const returnEnv = "HERDR_POPUP_RETURN"

// popupRef is a plugin's popup, as plugin.pane.open takes it.
type popupRef struct {
	PluginID   string            `json:"plugin_id"`
	Entrypoint string            `json:"entrypoint"`
	Width      string            `json:"width,omitempty"`
	Height     string            `json:"height,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
}

// A plugin's or an entrypoint's ID, as herdr allows them: what a popupRef
// read from the environment is held to.
var refID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func (p popupRef) valid() bool {
	return refID.MatchString(p.PluginID) && refID.MatchString(p.Entrypoint)
}

// cameFrom is the popup this one was opened from, if it was.
func cameFrom() *popupRef {
	var p popupRef
	if json.Unmarshal([]byte(os.Getenv(returnEnv)), &p) != nil || !p.valid() {
		return nil
	}
	return &p
}

// handoffWait is how long a popup is given to close before the next one's
// open is given up on.
const handoffWait = 4 * time.Second

// handOver closes this popup for another: it's what this program becomes
// once the popup's own terminal is restored (swap.go). A variable so tests
// can stand one in.
var handOver = func(p popupRef) error {
	if !p.valid() {
		return errors.New("not a popup: " + p.PluginID + "/" + p.Entrypoint)
	}
	pendingSwap = &p
	return nil
}

// handOverDetached opens another popup once this one has closed: by this
// program again, detached, since the popup's own process ends with it. For
// when becoming it (swap.go) can't be done.
func handOverDetached(p popupRef) error {
	if !p.valid() {
		return errors.New("not a popup: " + p.PluginID + "/" + p.Entrypoint)
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return spawnDetached("", "handoff", string(data))
}

// runHandoff is the detached half of handOver: it opens the popup as soon
// as herdr has room for it.
func runHandoff(arg string) error {
	var p popupRef
	if err := json.Unmarshal([]byte(arg), &p); err != nil {
		return err
	}
	if !p.valid() {
		return errors.New("not a popup: " + p.PluginID + "/" + p.Entrypoint)
	}
	params := map[string]any{
		"plugin_id": p.PluginID, "entrypoint": p.Entrypoint, "placement": "popup", "focus": true, "env": p.Env,
	}
	if p.Width != "" && p.Height != "" {
		params["width"], params["height"] = p.Width, p.Height
	}
	var err error
	for deadline := time.Now().Add(handoffWait); ; time.Sleep(60 * time.Millisecond) {
		if err = herdrCall("plugin.pane.open", params, nil); err == nil || !isHerdrCode(err, "ui_busy") || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		notify("Couldn't open " + p.PluginID + ": " + err.Error())
	}
	return err
}
