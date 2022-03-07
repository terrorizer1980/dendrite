// Copyright 2022 The Matrix.org Foundation C.I.C.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package routing

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	appserviceAPI "github.com/matrix-org/dendrite/appservice/api"
	"github.com/matrix-org/dendrite/clientapi/httputil"
	"github.com/matrix-org/dendrite/clientapi/jsonerror"
	"github.com/matrix-org/dendrite/internal/eventutil"
	roomserverAPI "github.com/matrix-org/dendrite/roomserver/api"
	"github.com/matrix-org/dendrite/setup/config"
	userapi "github.com/matrix-org/dendrite/userapi/api"
	userdb "github.com/matrix-org/dendrite/userapi/storage"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/util"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

type roomUpgradeReq struct {
	NewVersion gomatrixserverlib.RoomVersion `json:"new_version"`
}

type roomUpgradeRes struct {
	ReplacementRoom string `json:"replacement_room"`
}

func UpgradeRoom(
	req *http.Request,
	device *userapi.Device,
	rsAPI roomserverAPI.RoomserverInternalAPI,
	asAPI appserviceAPI.AppServiceQueryAPI,
	cfg *config.ClientAPI,
	accountDB userdb.Database,
	roomID string,
) util.JSONResponse {
	upgradeReq := &roomUpgradeReq{}
	parsed := httputil.UnmarshalJSONRequest(req, &upgradeReq)
	if parsed != nil {
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: jsonerror.Unknown("unable to parse upgrade request"),
		}
	}

	// verify new room version is supported
	if _, err := upgradeReq.NewVersion.EventFormat(); err != nil {
		return util.JSONResponse{
			Code: http.StatusBadRequest,
			JSON: jsonerror.UnsupportedRoomVersion(fmt.Sprintf("Unsupported room version: %s", err.Error())),
		}
	}
	ctx := req.Context()
	res := &roomserverAPI.QueryRoomVersionForRoomResponse{}
	err := rsAPI.QueryRoomVersionForRoom(ctx, &roomserverAPI.QueryRoomVersionForRoomRequest{RoomID: roomID}, res)
	if err != nil {
		return util.JSONResponse{
			Code: http.StatusNotFound,
			JSON: jsonerror.NotFound("unable to query room"),
		}
	}

	currentStateRes := &roomserverAPI.QueryLatestEventsAndStateResponse{}
	err = rsAPI.QueryLatestEventsAndState(ctx, &roomserverAPI.QueryLatestEventsAndStateRequest{RoomID: roomID}, currentStateRes)
	if err != nil {
		return util.JSONResponse{
			Code: http.StatusInternalServerError,
			JSON: jsonerror.Unknown("unable to query current state"),
		}
	}

	// pre generate a roomID, otherwise we can't send a tombstone event as createRoom didn't run yet
	newRoomID := fmt.Sprintf("!%s:%s", util.RandomString(16), cfg.Matrix.ServerName)

	// copy existing states
	var powerlevel []byte
	var hisVis string
	var joinRule string
	var topic string
	var name string
	var events []fledglingEvent
	var guestsCanJoin = true
	var canonicalAliasEvent map[string]interface{}
	for i := range currentStateRes.StateEvents {
		ev := currentStateRes.StateEvents[i]
		logrus.Debugf("State Event: %+v, %+v", string(ev.Content()), ev.Event)
		if ev.Type() == gomatrixserverlib.MRoomPowerLevels {
			pl, err := ev.PowerLevels()
			if err == nil {
				powerlevel = currentStateRes.StateEvents[i].Content()
				userLevel := pl.UserLevel(device.UserID)
				if pl.StateDefault > userLevel {
					return util.JSONResponse{
						Code: http.StatusForbidden,
						JSON: jsonerror.Forbidden("User is not allowed to set state event"),
					}
				}
			}
		}
		hisVisString, err := ev.HistoryVisibility()
		if err == nil {
			hisVis = hisVisString
		}
		joinRuleData, err := ev.JoinRule()
		if err == nil {
			joinRule = joinRuleData
		}
		if ev.Type() == gomatrixserverlib.MRoomCanonicalAlias && ev.StateKeyEquals("") {
			aliasEvent := roomserverAPI.AliasEvent{}
			if err := json.Unmarshal(ev.Content(), &aliasEvent); err != nil {
				return jsonerror.InternalServerError()
			}
			canonicalAliasEvent = map[string]interface{}{
				"alias":       aliasEvent.Alias,
				"alt_aliases": aliasEvent.AltAliases,
			}

		}
		switch ev.Type() {
		case gomatrixserverlib.MRoomTopic:
			topic = gjson.GetBytes(ev.Content(), "topic").Str
		case gomatrixserverlib.MRoomName:
			name = gjson.GetBytes(ev.Content(), "name").Str
		case gomatrixserverlib.MRoomGuestAccess:
			guestsCanJoin = !(gjson.GetBytes(ev.Content(), "guest_access").Str == "forbidden")
		case gomatrixserverlib.MRoomAvatar:
			fallthrough
		case "m.room.server_acl":
			fallthrough
		case gomatrixserverlib.MRoomEncryption:
			events = append(events, fledglingEvent{
				Type:     ev.Type(),
				StateKey: *ev.StateKey(),
				Content:  ev.Content(),
			})
		}
	}

	// build tombstone event
	builder := &gomatrixserverlib.EventBuilder{
		Sender:  device.UserID,
		RoomID:  roomID,
		Type:    "m.room.tombstone",
		Content: []byte(fmt.Sprintf(`{ "body": "This room has been replaced", "replacement_room": "%s" }`, newRoomID)),
	}

	eventsNeeded, err := gomatrixserverlib.StateNeededForEventBuilder(builder)
	if err != nil {
		logrus.WithError(err).Error("unable to get eventsNeeded")
		return jsonerror.InternalServerError()
	}
	if len(eventsNeeded.Tuples()) == 0 {
		logrus.WithError(err).Error("unable to get eventsNeeded 2")
		return jsonerror.InternalServerError()
	}
	newEvent, err := eventutil.BuildEvent(ctx, builder, cfg.Matrix, time.Now(), &eventsNeeded, currentStateRes)
	if err != nil {
		logrus.WithError(err).Error("build events failed")
		return jsonerror.InternalServerError()
	}

	err = roomserverAPI.SendEvents(ctx, rsAPI, roomserverAPI.KindNew, []*gomatrixserverlib.HeaderedEvent{newEvent}, cfg.Matrix.ServerName, cfg.Matrix.ServerName, nil, false)
	if err != nil {
		logrus.WithError(err).Error("roomserverAPI.SendEvents failed")
		return jsonerror.InternalServerError()
	}

	cr := createRoomRequest{
		RoomID:                    newRoomID,
		Name:                      name,
		Topic:                     topic,
		RoomVersion:               upgradeReq.NewVersion,
		PowerLevelContentOverride: powerlevel,
		Visibility:                hisVis,
		Preset:                    joinRule,
		InitialState:              events,
		GuestCanJoin:              guestsCanJoin,
		CreationContent:           []byte(fmt.Sprintf(`{ "predecessor": { "room_id": "%s", "event_id": "%s" } }`, roomID, newEvent.EventID())),
	}

	logrus.Debugf("createRoomRequest: %+v", cr)
	createRoomRes := createRoom(ctx, cr, device, cfg, accountDB, rsAPI, asAPI, time.Now())

	// old room aliases
	aliases := &roomserverAPI.GetAliasesForRoomIDResponse{}
	if err := rsAPI.GetAliasesForRoomID(ctx, &roomserverAPI.GetAliasesForRoomIDRequest{RoomID: roomID}, aliases); err != nil {
		logrus.WithError(err).Error("roomserverAPI.GetAliasesForRoomID failed")
		return jsonerror.InternalServerError()
	}

	// move aliases to new room
	for _, alias := range aliases.Aliases {
		delAliasRes := &roomserverAPI.RemoveRoomAliasResponse{}
		err := rsAPI.RemoveRoomAlias(ctx, &roomserverAPI.RemoveRoomAliasRequest{Alias: alias, UserID: device.UserID, Purge: true}, delAliasRes)
		if err != nil {
			logrus.WithError(err).Error("roomserverAPI.RemoveRoomAlias failed")
			continue
		}
		res := &roomserverAPI.SetRoomAliasResponse{}
		err = rsAPI.SetRoomAlias(ctx, &roomserverAPI.SetRoomAliasRequest{Alias: alias, RoomID: newRoomID, UserID: device.UserID}, res)
		if err != nil {
			logrus.WithError(err).Error("roomserverAPI.SetRoomAlias failed")
			continue
		}
	}

	// set the canonical alias event
	if canonicalAliasEvent != nil {
		stateKey := ""
		ev2, err := generateSendEvent(ctx, canonicalAliasEvent, device, newRoomID, gomatrixserverlib.MRoomCanonicalAlias, &stateKey, cfg, rsAPI, time.Now())
		if err != nil {
			logrus.Errorf("unable to generate send event: %+v", err)
			return *err
		}
		logrus.Debugf("New alias event: %+v", string(ev2.Content()))
		if err := roomserverAPI.SendEvents(
			req.Context(), rsAPI,
			roomserverAPI.KindNew,
			[]*gomatrixserverlib.HeaderedEvent{
				ev2.Headered(upgradeReq.NewVersion),
			},
			cfg.Matrix.ServerName,
			cfg.Matrix.ServerName,
			nil,
			false,
		); err != nil {
			util.GetLogger(req.Context()).WithError(err).Error("SendEvents failed")
			return jsonerror.InternalServerError()
		}
	}

	switch createRoomRes.JSON.(type) {
	case createRoomResponse:

		return util.JSONResponse{
			Code: http.StatusOK,
			JSON: roomUpgradeRes{
				ReplacementRoom: newRoomID,
			},
		}
	default:
		return createRoomRes
	}
}
