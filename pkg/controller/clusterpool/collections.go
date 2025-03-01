package clusterpool

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	log "github.com/sirupsen/logrus"

	corev1 "k8s.io/api/core/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hivev1 "github.com/openshift/hive/apis/hive/v1"
	"github.com/openshift/hive/pkg/constants"
	"github.com/openshift/hive/pkg/controller/utils"
	controllerutils "github.com/openshift/hive/pkg/controller/utils"
)

type claimCollection struct {
	// All claims for this pool
	byClaimName map[string]*hivev1.ClusterClaim
	unassigned  []*hivev1.ClusterClaim
	// This contains only assigned claims
	byCDName map[string]*hivev1.ClusterClaim
}

// getAllClaimsForPool is the constructor for a claimCollection for all of the
// ClusterClaims that are requesting clusters from the specified pool.
func getAllClaimsForPool(c client.Client, pool *hivev1.ClusterPool, logger log.FieldLogger) (*claimCollection, error) {
	claimsList := &hivev1.ClusterClaimList{}
	if err := c.List(
		context.Background(), claimsList,
		client.MatchingFields{claimClusterPoolIndex: pool.Name},
		client.InNamespace(pool.Namespace)); err != nil {
		logger.WithError(err).Error("error listing ClusterClaims")
		return nil, err
	}
	claimCol := claimCollection{
		byClaimName: make(map[string]*hivev1.ClusterClaim),
		unassigned:  make([]*hivev1.ClusterClaim, 0),
		byCDName:    make(map[string]*hivev1.ClusterClaim),
	}
	for i, claim := range claimsList.Items {
		// skip claims for other pools
		// This should only happen in unit tests: the fakeclient doesn't support index filters
		if claim.Spec.ClusterPoolName != pool.Name {
			logger.WithFields(log.Fields{
				"claim":         claim.Name,
				"claimPool":     claim.Spec.ClusterPoolName,
				"reconcilePool": pool.Name,
			}).Error("unepectedly got a ClusterClaim not belonging to this pool")
			continue
		}
		ref := &claimsList.Items[i]
		claimCol.byClaimName[claim.Name] = ref
		if cdName := claim.Spec.Namespace; cdName == "" {
			claimCol.unassigned = append(claimCol.unassigned, ref)
		} else {
			// TODO: Though it should be impossible without manual intervention, if multiple claims
			// ref the same CD, whichever comes last in the list will "win". If this is deemed
			// important enough to worry about, consider making byCDName a map[string][]*Claim
			// instead.
			claimCol.byCDName[cdName] = ref
		}
	}
	// Sort assignable claims by creationTimestamp for FIFO behavior.
	sort.Slice(
		claimCol.unassigned,
		func(i, j int) bool {
			return claimCol.unassigned[i].CreationTimestamp.Before(&claimCol.unassigned[j].CreationTimestamp)
		},
	)

	logger.WithFields(log.Fields{
		"assignedCount":   len(claimCol.byCDName),
		"unassignedCount": len(claimCol.unassigned),
	}).Debug("found claims for ClusterPool")

	return &claimCol, nil
}

// ByName returns the named claim from the collection, or nil if no claim by that name exists.
func (c *claimCollection) ByName(claimName string) *hivev1.ClusterClaim {
	claim, _ := c.byClaimName[claimName]
	return claim
}

// Unassigned returns a list of claims that are not assigned to clusters yet. The list is sorted by
// age, oldest first.
func (c *claimCollection) Unassigned() []*hivev1.ClusterClaim {
	return c.unassigned
}

// Assign assigns the specified claim to the specified cluster, updating its spec and status on
// the server. Errors updating the spec or status are bubbled up. Returns an error if the claim is
// already assigned (to *any* CD). Does *not* validate that the CD isn't already assigned (to this
// or another claim).
func (claims *claimCollection) Assign(c client.Client, claim *hivev1.ClusterClaim, cd *hivev1.ClusterDeployment) error {
	if claim.Spec.Namespace != "" {
		return fmt.Errorf("Claim %s is already assigned to %s. This is a bug!", claim.Name, claim.Spec.Namespace)
	}
	for i, claimi := range claims.unassigned {
		if claimi.Name == claim.Name {
			// Update the spec
			claimi.Spec.Namespace = cd.Namespace
			if err := c.Update(context.Background(), claimi); err != nil {
				return err
			}
			// Update the status
			claimi.Status.Conditions = controllerutils.SetClusterClaimCondition(
				claimi.Status.Conditions,
				hivev1.ClusterClaimPendingCondition,
				corev1.ConditionTrue,
				"ClusterAssigned",
				"Cluster assigned to ClusterClaim, awaiting claim",
				controllerutils.UpdateConditionIfReasonOrMessageChange,
			)
			if err := c.Status().Update(context.Background(), claimi); err != nil {
				return err
			}
			// "Move" the claim from the unassigned list to the assigned map.
			// (unassigned remains sorted as this is a removal)
			claims.byCDName[claim.Spec.Namespace] = claimi
			copy(claims.unassigned[i:], claims.unassigned[i+1:])
			claims.unassigned = claims.unassigned[:len(claims.unassigned)-1]
			return nil
		}
	}
	return fmt.Errorf("Claim %s is not assigned, but was not found in the unassigned list. This is a bug!", claim.Name)
}

// SyncClusterDeploymentAssignments makes sure each claim which purports to be assigned has the
// correct CD assigned to it, updating the CD and/or claim on the server as necessary.
func (claims *claimCollection) SyncClusterDeploymentAssignments(c client.Client, cds *cdCollection, logger log.FieldLogger) {
	invalidCDs := []string{}
	claimsToRemove := []string{}
	for cdName, claim := range claims.byCDName {
		cd := cds.ByName(cdName)
		logger := logger.WithFields(log.Fields{
			"ClusterDeployment": cdName,
			"Claim":             claim.Name,
		})
		if cd == nil {
			logger.Error("couldn't sync ClusterDeployment to the claim assigned to it: ClusterDeployment not found")
		} else if err := ensureClaimAssignment(c, claim, claims, cd, cds, logger); err != nil {
			logger.WithError(err).Error("couldn't sync ClusterDeployment to the claim assigned to it")
		} else {
			// Happy path
			continue
		}
		// The claim and CD are no good; remove them from further consideration
		invalidCDs = append(invalidCDs, cdName)
		claimsToRemove = append(claimsToRemove, claim.Name)
	}
	cds.MakeNotAssignable(invalidCDs...)
	claims.Untrack(claimsToRemove...)
}

// Untrack removes the named claims from the claimCollection, so they are no longer
// - returned via ByName() or Unassigned()
// - available for Assign() or affected by SyncClusterDeploymentAssignments
// Do this to broken claims.
func (c *claimCollection) Untrack(claimNames ...string) {
	for _, claimName := range claimNames {
		found, ok := c.byClaimName[claimName]
		if !ok {
			// Count on consistency: if it's not in one collection, it's not in any of them
			return
		}
		delete(c.byClaimName, claimName)
		for i, claim := range c.unassigned {
			if claim.Name == claimName {
				copy(c.unassigned[i:], c.unassigned[i+1:])
				c.unassigned = c.unassigned[:len(c.unassigned)-1]
			}
		}
		if cdName := found.Spec.Namespace; cdName != "" {
			// TODO: Should just be able to
			// 		delete(c.byCDName, cdName)
			// but it's theoretically possible multiple claims ref the same CD, so double check that
			// this is the right one.
			if toRemove, ok := c.byCDName[cdName]; ok && toRemove.Name == claimName {
				delete(c.byCDName, cdName)
			}
		}
	}
}

type cdCollection struct {
	// Unclaimed installed clusters which belong to this pool and are not (marked for) deleting
	assignable []*hivev1.ClusterDeployment
	// Unclaimed installing clusters which belong to this pool and are not (marked for) deleting
	installing []*hivev1.ClusterDeployment
	// Clusters with a DeletionTimestamp. Mutually exclusive with markedForDeletion.
	deleting []*hivev1.ClusterDeployment
	// Clusters with the ClusterClaimRemoveClusterAnnotation. Mutually exclusive with deleting.
	markedForDeletion []*hivev1.ClusterDeployment
	// All CDs in this pool
	byCDName map[string]*hivev1.ClusterDeployment
	// This contains only claimed CDs
	byClaimName map[string]*hivev1.ClusterDeployment
}

// getAllClusterDeploymentsForPool is the constructor for a cdCollection
// comprising all the ClusterDeployments created for the specified ClusterPool.
func getAllClusterDeploymentsForPool(c client.Client, pool *hivev1.ClusterPool, logger log.FieldLogger) (*cdCollection, error) {
	cdList := &hivev1.ClusterDeploymentList{}
	if err := c.List(context.Background(), cdList,
		client.MatchingFields{cdClusterPoolIndex: poolKey(pool.GetNamespace(), pool.GetName())}); err != nil {
		logger.WithError(err).Error("error listing ClusterDeployments")
		return nil, err
	}
	cdCol := cdCollection{
		assignable:  make([]*hivev1.ClusterDeployment, 0),
		installing:  make([]*hivev1.ClusterDeployment, 0),
		deleting:    make([]*hivev1.ClusterDeployment, 0),
		byCDName:    make(map[string]*hivev1.ClusterDeployment),
		byClaimName: make(map[string]*hivev1.ClusterDeployment),
	}
	for i, cd := range cdList.Items {
		poolRef := cd.Spec.ClusterPoolRef
		if poolRef == nil || poolRef.Namespace != pool.Namespace || poolRef.PoolName != pool.Name {
			// This should only happen in unit tests: the fakeclient doesn't support index filters
			logger.WithFields(log.Fields{
				"ClusterDeployment": cd.Name,
				"Pool":              pool.Name,
				"CD.PoolRef":        cd.Spec.ClusterPoolRef,
			}).Error("unepectedly got a ClusterDeployment not belonging to this pool")
			continue
		}
		ref := &cdList.Items[i]
		cdCol.byCDName[cd.Name] = ref
		claimName := poolRef.ClaimName
		if ref.DeletionTimestamp != nil {
			cdCol.deleting = append(cdCol.deleting, ref)
		} else if controllerutils.IsClaimedClusterMarkedForRemoval(ref) {
			// Do *not* double count "deleting" and "marked for deletion"
			cdCol.markedForDeletion = append(cdCol.markedForDeletion, ref)
		} else if claimName == "" {
			if cd.Spec.Installed {
				cdCol.assignable = append(cdCol.assignable, ref)
			} else {
				cdCol.installing = append(cdCol.installing, ref)
			}
		}
		// Register all claimed CDs, even if they're deleting/marked
		if claimName != "" {
			// TODO: Though it should be impossible without manual intervention, if multiple CDs
			// ref the same claim, whichever comes last in the list will "win". If this is deemed
			// important enough to worry about, consider making byClaimName a map[string][]*CD
			// instead.
			cdCol.byClaimName[claimName] = ref
		}
	}
	// Sort assignable CDs so we assign them in FIFO order
	sort.Slice(
		cdCol.assignable,
		func(i, j int) bool {
			return cdCol.assignable[i].CreationTimestamp.Before(&cdCol.assignable[j].CreationTimestamp)
		},
	)
	// Sort installing CDs so we prioritize deleting those that are furthest away from completing
	// their installation (prioritizing preserving those that will be assignable the soonest).
	sort.Slice(
		cdCol.installing,
		func(i, j int) bool {
			return cdCol.installing[i].CreationTimestamp.After(cdCol.installing[j].CreationTimestamp.Time)
		},
	)

	logger.WithFields(log.Fields{
		"assignable": len(cdCol.assignable),
		"claimed":    len(cdCol.byClaimName),
		"deleting":   len(cdCol.deleting),
		"installing": len(cdCol.installing),
		"unclaimed":  len(cdCol.installing) + len(cdCol.assignable),
	}).Debug("found clusters for ClusterPool")
	return &cdCol, nil
}

// ByName returns the named ClusterDeployment from the cdCollection, or nil if no CD by that name exists.
func (cds *cdCollection) ByName(cdName string) *hivev1.ClusterDeployment {
	cd, _ := cds.byCDName[cdName]
	return cd

}

// Total returns the total number of ClusterDeployments in the cdCollection.
func (cds *cdCollection) Total() int {
	return len(cds.byCDName)
}

// NumAssigned returns the number of ClusterDeployments assigned to claims.
func (cds *cdCollection) NumAssigned() int {
	return len(cds.byClaimName)
}

// Assignable returns a list of ClusterDeployment refs, sorted by creationTimestamp
func (cds *cdCollection) Assignable() []*hivev1.ClusterDeployment {
	return cds.assignable
}

// Deleting returns the list of ClusterDeployments whose DeletionTimestamp is set. Not to be
// confused with MarkedForDeletion.
func (cds *cdCollection) Deleting() []*hivev1.ClusterDeployment {
	return cds.deleting
}

// MarkedForDeletion returns the list of ClusterDeployments with the
// ClusterClaimRemoveClusterAnnotation. Not to be confused with Deleting: if a CD has its
// DeletionTimestamp set, it is *not* included in MarkedForDeletion.
func (cds *cdCollection) MarkedForDeletion() []*hivev1.ClusterDeployment {
	return cds.markedForDeletion
}

// Installing returns the list of ClusterDeployments in the process of being installed. These are
// not available for claim assignment.
func (cds *cdCollection) Installing() []*hivev1.ClusterDeployment {
	return cds.installing
}

// Assign assigns the specified ClusterDeployment to the specified claim, updating its spec on the
// server. Errors from the update are bubbled up. Returns an error if the CD is already assigned
// (to *any* claim). The CD must be from the Assignable() list; otherwise it is an error.
func (cds *cdCollection) Assign(c client.Client, cd *hivev1.ClusterDeployment, claim *hivev1.ClusterClaim) error {
	if cd.Spec.ClusterPoolRef.ClaimName != "" {
		return fmt.Errorf("ClusterDeployment %s is already assigned to %s. This is a bug!", cd.Name, cd.Spec.ClusterPoolRef.ClaimName)
	}
	// "Move" the cd from assignable to byClaimName
	for i, cdi := range cds.assignable {
		if cdi.Name == cd.Name {
			// Update the spec
			cdi.Spec.ClusterPoolRef.ClaimName = claim.Name
			cdi.Spec.PowerState = hivev1.RunningClusterPowerState
			if err := c.Update(context.Background(), cdi); err != nil {
				return err
			}
			// "Move" the CD from the assignable list to the assigned map
			cds.byClaimName[cd.Spec.ClusterPoolRef.ClaimName] = cdi
			copy(cds.assignable[i:], cds.assignable[i+1:])
			cds.assignable = cds.assignable[:len(cds.assignable)-1]
			return nil
		}
	}
	return fmt.Errorf("ClusterDeployment %s is not assigned, but was not found in the assignable list. This is a bug!", cd.Name)
}

// SyncClaimAssignments makes sure each ClusterDeployment which purports to be assigned has the
// correct claim assigned to it, updating the CD and/or claim on the server as necessary.
func (cds *cdCollection) SyncClaimAssignments(c client.Client, claims *claimCollection, logger log.FieldLogger) {
	claimsToRemove := []string{}
	invalidCDs := []string{}
	for claimName, cd := range cds.byClaimName {
		logger := logger.WithFields(log.Fields{
			"Claim":             claimName,
			"ClusterDeployment": cd.Name,
		})
		if claim := claims.ByName(claimName); claim == nil {
			logger.Error("couldn't sync ClusterClaim to the ClusterDeployment assigned to it: Claim not found")
		} else if err := ensureClaimAssignment(c, claim, claims, cd, cds, logger); err != nil {
			logger.WithError(err).Error("couldn't sync ClusterClaim to the ClusterDeployment assigned to it")
		} else {
			// Happy path
			continue
		}
		// The claim and CD are no good; remove them from further consideration
		claimsToRemove = append(claimsToRemove, claimName)
		invalidCDs = append(invalidCDs, cd.Name)
	}
	claims.Untrack(claimsToRemove...)
	cds.MakeNotAssignable(invalidCDs...)
}

func removeCDsFromSlice(slice *[]*hivev1.ClusterDeployment, cdNames ...string) {
	for _, cdName := range cdNames {
		for i, cd := range *slice {
			if cd.Name == cdName {
				copy((*slice)[i:], (*slice)[i+1:])
				*slice = (*slice)[:len(*slice)-1]
			}
		}
	}
}

// MakeNotAssignable idempotently removes the named ClusterDeployments from the assignable list of the
// cdCollection, so they are no longer considered for assignment. They still count against pool
// capacity. Do this to a broken ClusterDeployment -- e.g. one that
// - is assigned to the wrong claim, or a claim that doesn't exist
// - can't be synced with its claim for whatever reason in this iteration (e.g. Update() failure)
func (cds *cdCollection) MakeNotAssignable(cdNames ...string) {
	removeCDsFromSlice(&cds.assignable, cdNames...)
}

// Delete deletes the named ClusterDeployment from the server, moving it from Assignable() to
// Deleting()
func (cds *cdCollection) Delete(c client.Client, cdName string) error {
	cd := cds.ByName(cdName)
	if cd == nil {
		return errors.New(fmt.Sprintf("No such ClusterDeployment %s to delete. This is a bug!", cdName))
	}
	if err := utils.SafeDelete(c, context.Background(), cd); err != nil {
		return err
	}
	cds.deleting = append(cds.deleting, cd)
	// Remove from any of the other lists it might be in
	removeCDsFromSlice(&cds.assignable, cdName)
	removeCDsFromSlice(&cds.installing, cdName)
	removeCDsFromSlice(&cds.assignable, cdName)
	removeCDsFromSlice(&cds.markedForDeletion, cdName)
	return nil
}

// setCDsCurrentCondition idempotently sets the ClusterDeploymentsCurrent condition on the
// ClusterPool according to whether all unassigned CDs have the same PoolVersion as the pool.
func setCDsCurrentCondition(c client.Client, cds *cdCollection, clp *hivev1.ClusterPool, poolVersion string) error {
	// CDs with mismatched poolVersion
	mismatchedCDs := make([]string, 0)
	// CDs with no poolVersion
	unknownCDs := make([]string, 0)

	for _, cd := range append(cds.Assignable(), cds.Installing()...) {
		if cdPoolVersion, ok := cd.Annotations[constants.ClusterDeploymentPoolSpecHashAnnotation]; !ok || cdPoolVersion == "" {
			// Annotation is either missing or empty. This could be due to upgrade (this CD was
			// created before this code was installed) or manual intervention (outside agent mucked
			// with the annotation). Either way we don't know whether the CD matches or not.
			unknownCDs = append(unknownCDs, cd.Name)
		} else if cdPoolVersion != poolVersion {
			mismatchedCDs = append(mismatchedCDs, cd.Name)
		}
	}

	var status corev1.ConditionStatus
	var reason, message string
	if len(mismatchedCDs) != 0 {
		// We can assert staleness if there are any mismatches
		status = corev1.ConditionFalse
		reason = "SomeClusterDeploymentsStale"
		sort.Strings(mismatchedCDs)
		message = fmt.Sprintf("Some unassigned ClusterDeployments do not match the pool configuration: %s", strings.Join(mismatchedCDs, ", "))
	} else if len(unknownCDs) != 0 {
		// There are no mismatches, but some unknowns. Note that this is a different "unknown" from "we haven't looked yet".
		status = corev1.ConditionUnknown
		reason = "SomeClusterDeploymentsUnknown"
		sort.Strings(unknownCDs)
		message = fmt.Sprintf("Some unassigned ClusterDeployments are missing their pool spec hash annotation: %s", strings.Join(unknownCDs, ", "))
	} else {
		// All match (or there are no CDs, which is also fine)
		status = corev1.ConditionTrue
		reason = "ClusterDeploymentsCurrent"
		message = "All unassigned ClusterDeployments match the pool configuration"
	}

	// This will re-update with the same status/reason multiple times as stale/unknown CDs get
	// claimed. That's intentional.
	conds, changed := controllerutils.SetClusterPoolConditionWithChangeCheck(
		clp.Status.Conditions,
		hivev1.ClusterPoolAllClustersCurrentCondition,
		status, reason, message, controllerutils.UpdateConditionIfReasonOrMessageChange)
	if changed {
		clp.Status.Conditions = conds
		if err := c.Status().Update(context.Background(), clp); err != nil {
			return err
		}
	}
	return nil
}

// ensureClaimAssignment returns successfully (nil) when the claim and the cd are both assigned to each other.
// If a non-nil error is returned, it could mean anything else, including:
// - We were given bad parameters
// - We tried to update the claim and/or the cd but failed
func ensureClaimAssignment(c client.Client, claim *hivev1.ClusterClaim, claims *claimCollection, cd *hivev1.ClusterDeployment, cds *cdCollection, logger log.FieldLogger) error {
	poolRefInCD := cd.Spec.ClusterPoolRef

	// These should never happen. If they do, it's a programmer error. The caller should only be
	// processing CDs in the same pool as the claim, which means ClusterPoolRef is a) populated,
	// and b) matches the claim's pool.
	if poolRefInCD == nil {
		return errors.New("unexpectedly got a ClusterDeployment with no ClusterPoolRef")
	}
	if poolRefInCD.Namespace != claim.Namespace || poolRefInCD.PoolName != claim.Spec.ClusterPoolName {
		return fmt.Errorf("unexpectedly got a ClusterDeployment and a ClusterClaim in different pools. "+
			"ClusterDeployment %s is in pool %s/%s; "+
			"ClusterClaim %s is in pool %s/%s",
			cd.Name, poolRefInCD.Namespace, poolRefInCD.PoolName,
			claim.Name, claim.Namespace, claim.Spec.ClusterPoolName)
	}

	// These should be nearly impossible, but may result from a timing issue (or an explicit update by a user?)
	if poolRefInCD.ClaimName != "" && poolRefInCD.ClaimName != claim.Name {
		return fmt.Errorf("conflict: ClusterDeployment %s is assigned to ClusterClaim %s (expected %s)",
			cd.Name, poolRefInCD.ClaimName, claim.Name)
	}
	if claim.Spec.Namespace != "" && claim.Spec.Namespace != cd.Namespace {
		// The clusterclaim_controller will eventually set the Pending/AssignmentConflict condition on this claim
		return fmt.Errorf("conflict: ClusterClaim %s is assigned to ClusterDeployment %s (expected %s)",
			claim.Name, claim.Spec.Namespace, cd.Namespace)
	}

	logger = logger.WithField("claim", claim.Name).WithField("cluster", cd.Namespace)
	logger.Debug("ensuring cluster <=> claim assignment")

	// Update the claim first
	if claim.Spec.Namespace == "" {
		logger.Info("updating claim to assign cluster")
		if err := claims.Assign(c, claim, cd); err != nil {
			return err
		}
	} else {
		logger.Debug("claim already assigned")
	}

	// Now update the CD
	if poolRefInCD.ClaimName == "" {
		logger.Info("updating cluster to assign claim")
		if err := cds.Assign(c, cd, claim); err != nil {
			return err
		}
	} else {
		logger.Debug("cluster already assigned")
	}

	logger.Debug("cluster <=> claim assignment ok")
	return nil
}

// assignClustersToClaims iterates over unassigned claims and assignable ClusterDeployments, in order (see
// claimCollection.Unassigned and cdCollection.Assignable), assigning them to each other, stopping when the
// first of the two lists is exhausted.
func assignClustersToClaims(c client.Client, claims *claimCollection, cds *cdCollection, logger log.FieldLogger) error {
	// ensureClaimAssignment modifies claims.unassigned and cds.assignable, so make a copy of the lists.
	// copy() limits itself to the size of the destination
	numToAssign := minIntVarible(len(claims.Unassigned()), len(cds.Assignable()))
	claimList := make([]*hivev1.ClusterClaim, numToAssign)
	copy(claimList, claims.Unassigned())
	cdList := make([]*hivev1.ClusterDeployment, numToAssign)
	copy(cdList, cds.Assignable())
	var errs []error
	for i := 0; i < numToAssign; i++ {
		if err := ensureClaimAssignment(c, claimList[i], claims, cdList[i], cds, logger); err != nil {
			errs = append(errs, err)
		}
	}
	// If any unassigned claims remain, mark their status accordingly
	for _, claim := range claims.Unassigned() {
		logger := logger.WithField("claim", claim.Name)
		logger.Debug("no clusters ready to assign to claim")
		if conds, statusChanged := controllerutils.SetClusterClaimConditionWithChangeCheck(
			claim.Status.Conditions,
			hivev1.ClusterClaimPendingCondition,
			corev1.ConditionTrue,
			"NoClusters",
			"No clusters in pool are ready to be claimed",
			controllerutils.UpdateConditionIfReasonOrMessageChange,
		); statusChanged {
			claim.Status.Conditions = conds
			if err := c.Status().Update(context.Background(), claim); err != nil {
				logger.WithError(err).Log(controllerutils.LogLevel(err), "could not update status of ClusterClaim")
				errs = append(errs, err)
			}
		}
	}
	return utilerrors.NewAggregate(errs)
}
