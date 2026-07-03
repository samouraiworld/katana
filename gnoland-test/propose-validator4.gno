// propose-validator4.gno.tmpl - rendered to propose-validator4.gno by bootstrap.sh.
// The placeholder below is replaced with the freshly generated `v4op` operator
// address, so this script stays correct even though keys are regenerated.
package main

import (
	"gno.land/r/gov/dao"
	"gno.land/r/gnops/valopers/proposal"
)

func main(cur realm) {
	pr := proposal.NewValidatorProposalRequest(cross(cur), address("g1ps3yvz3fjkmpng8p5jaysjd92uh73z5s6trv59"))
	dao.MustCreateProposal(cross(cur), pr)
}
