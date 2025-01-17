// mautrix-signal - A Matrix-signal puppeting bridge.
// Copyright (C) 2023 Scott Weber
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package signalmeow

const (
	UUID_KIND_ACI = "aci"
	UUID_KIND_PNI = "pni"
)

type UUIDKind string

type GroupCredentials struct {
	Credentials []GroupCredential `json:"credentials"`
	Pni         string            `json:"pni"`
}
type GroupCredential struct {
	Credential     []byte
	RedemptionTime int64
}
type GroupExternalCredential struct {
	Token []byte `json:"token"`
}
