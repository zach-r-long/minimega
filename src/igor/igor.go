// Copyright (2019) Sandia Corporation.
// Under the terms of Contract DE-AC04-94AL85000 with Sandia Corporation,
// the U.S. Government retains certain rights in this software.
package main

import "os/user"

// igor holds globals
var igor struct {
	Config       // embed
	Reservations // embed
	Backend      // embed
	*user.User   // embed
}
