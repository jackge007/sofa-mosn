/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package server

import (
	"fmt"

	"github.com/alipay/sofa-mosn/pkg/admin/store"
	"github.com/alipay/sofa-mosn/pkg/api/v2"
	"github.com/alipay/sofa-mosn/pkg/log"
	"github.com/alipay/sofa-mosn/pkg/metrics"
	"github.com/alipay/sofa-mosn/pkg/metrics/sink/console"
	"github.com/alipay/sofa-mosn/pkg/server"
	"github.com/valyala/fasthttp"
)

var levelMap = map[string]log.Level{
	"FATAL": log.FATAL,
	"ERROR": log.ERROR,
	"WARN":  log.WARN,
	"INFO":  log.INFO,
	"DEBUG": log.DEBUG,
	"TRACE": log.TRACE,
}

const errMsgFmt = `{
	"error": "%s"
}
`

func configDump(ctx *fasthttp.RequestCtx) {
	if buf, err := store.Dump(); err == nil {
		ctx.Write(buf)
	} else {
		ctx.SetStatusCode(500)
		msg := fmt.Sprintf(errMsgFmt, "internal error")
		ctx.WriteString(msg)
		log.DefaultLogger.Errorf("Admin API: ConfigDump failed, cause by %s", err)
	}
}

func statsDump(ctx *fasthttp.RequestCtx) {
	sink := console.NewConsoleSink(ctx.Response.BodyWriter())
	sink.Flush(metrics.GetAll())
}

func setLogLevel(ctx *fasthttp.RequestCtx) {
	body := string(ctx.Request.Body())
	if level, ok := levelMap[body]; ok {
		log.DefaultLogger.Level = level
		log.DefaultLogger.Infof("DefaultLogger level has been changed to %s", body)
		ctx.WriteString("update logger success\n")
	} else {
		ctx.SetStatusCode(500)
		msg := fmt.Sprintf(errMsgFmt, "unknown log level")
		ctx.WriteString(msg)
	}
}

// POST Data Format
/*
{
	"listener": "string",
	"inspector": bool,
	"tls_context": {}
}
*/
type tlsUpdate struct {
	Listener  string       `json:"listener"`
	Inspetcor bool         `json:"inspetcor"`
	TLSConfig v2.TLSConfig `json:"tls_context"`
}

func updateListenerTLS(ctx *fasthttp.RequestCtx) {
	body := ctx.Request.Body()
	data := &tlsUpdate{}
	if err := json.Unmarshal(body, data); err != nil {
		ctx.SetStatusCode(400)
		ctx.Write([]byte(`{ error: "invalid post data"}`))
		return
	}
	adapter := server.GetListenerAdapterInstance()
	// server can be "", so use the default server. currently we only support one server, so use "" is ok
	if err := adapter.UpdateListenerTLS("", data.Listener, data.Inspetcor, &data.TLSConfig); err != nil {
		ctx.SetStatusCode(500)
		msg := ""
		ctx.Write([]byte(msg))
		return
	}
	log.DefaultLogger.Infof("listener %s's tls config has been changed, inspector: %v, tlsstart: %v", data.Listener, data.Inspetcor, data.TLSConfig.Status)
	ctx.WriteString("update tls success\n")
}