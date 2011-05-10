# Copyright 2011 Evan Shaw. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

include $(GOROOT)/src/Make.inc

TARG=github.com/edsrzf/zegomq
GOFILES=\
	msg.go\
	socket.go\
	zmq.go\

include $(GOROOT)/src/Make.pkg
