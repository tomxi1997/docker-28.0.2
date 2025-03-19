package bridge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"

	"github.com/containerd/log"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/internal/nlwrap"
	"github.com/docker/docker/libnetwork/iptables"
	"github.com/docker/docker/libnetwork/types"
	"github.com/vishvananda/netlink"
)

// DockerChain: DOCKER iptable chain name
const (
	DockerChain        = "DOCKER"
	DockerForwardChain = "DOCKER-FORWARD"
	DockerBridgeChain  = "DOCKER-BRIDGE"
	DockerCTChain      = "DOCKER-CT"

	// Isolation between bridge networks is achieved in two stages by means
	// of the following two chains in the filter table. The first chain matches
	// on the source interface being a bridge network's bridge and the
	// destination being a different interface. A positive match leads to the
	// second isolation chain. No match returns to the parent chain. The second
	// isolation chain matches on destination interface being a bridge network's
	// bridge. A positive match identifies a packet originated from one bridge
	// network's bridge destined to another bridge network's bridge and will
	// result in the packet being dropped. No match returns to the parent chain.

	IsolationChain1 = "DOCKER-ISOLATION-STAGE-1"
	IsolationChain2 = "DOCKER-ISOLATION-STAGE-2"
)

// Path to the executable installed in Linux under WSL2 that reports on
// WSL config. https://github.com/microsoft/WSL/releases/tag/2.0.4
// Can be modified by tests.
var wslinfoPath = "/usr/bin/wslinfo"

func setupIPChains(config configuration, version iptables.IPVersion) (retErr error) {
	// Sanity check.
	if version == iptables.IPv4 && !config.EnableIPTables {
		return errors.New("cannot create new chains, iptables is disabled")
	}
	if version == iptables.IPv6 && !config.EnableIP6Tables {
		return errors.New("cannot create new chains, ip6tables is disabled")
	}

	iptable := iptables.GetIptable(version)

	_, err := iptable.NewChain(DockerChain, iptables.Nat)
	if err != nil {
		return fmt.Errorf("failed to create NAT chain %s: %v", DockerChain, err)
	}
	defer func() {
		if retErr != nil {
			if err := iptable.RemoveExistingChain(DockerChain, iptables.Nat); err != nil {
				log.G(context.TODO()).Warnf("failed on removing iptables NAT chain %s on cleanup: %v", DockerChain, err)
			}
		}
	}()

	_, err = iptable.NewChain(DockerChain, iptables.Filter)
	if err != nil {
		return fmt.Errorf("failed to create FILTER chain %s: %v", DockerChain, err)
	}
	defer func() {
		if retErr != nil {
			if err := iptable.RemoveExistingChain(DockerChain, iptables.Filter); err != nil {
				log.G(context.TODO()).Warnf("failed on removing iptables FILTER chain %s on cleanup: %v", DockerChain, err)
			}
		}
	}()

	_, err = iptable.NewChain(DockerForwardChain, iptables.Filter)
	if err != nil {
		return fmt.Errorf("failed to create FILTER chain %s: %v", DockerForwardChain, err)
	}
	defer func() {
		if retErr != nil {
			if err := iptable.RemoveExistingChain(DockerForwardChain, iptables.Filter); err != nil {
				log.G(context.TODO()).Warnf("failed on removing iptables FILTER chain %s on cleanup: %v", DockerForwardChain, err)
			}
		}
	}()

	_, err = iptable.NewChain(DockerBridgeChain, iptables.Filter)
	if err != nil {
		return fmt.Errorf("failed to create FILTER chain %s: %v", DockerBridgeChain, err)
	}
	defer func() {
		if retErr != nil {
			if err := iptable.RemoveExistingChain(DockerBridgeChain, iptables.Filter); err != nil {
				log.G(context.TODO()).Warnf("failed on removing iptables FILTER chain %s on cleanup: %v", DockerBridgeChain, err)
			}
		}
	}()

	_, err = iptable.NewChain(DockerCTChain, iptables.Filter)
	if err != nil {
		return fmt.Errorf("failed to create FILTER chain %s: %v", DockerCTChain, err)
	}
	defer func() {
		if retErr != nil {
			if err := iptable.RemoveExistingChain(DockerCTChain, iptables.Filter); err != nil {
				log.G(context.TODO()).Warnf("failed on removing iptables FILTER chain %s on cleanup: %v", DockerCTChain, err)
			}
		}
	}()

	_, err = iptable.NewChain(IsolationChain1, iptables.Filter)
	if err != nil {
		return fmt.Errorf("failed to create FILTER isolation chain: %v", err)
	}
	defer func() {
		if retErr != nil {
			if err := iptable.RemoveExistingChain(IsolationChain1, iptables.Filter); err != nil {
				log.G(context.TODO()).Warnf("failed on removing iptables FILTER chain %s on cleanup: %v", IsolationChain1, err)
			}
		}
	}()

	_, err = iptable.NewChain(IsolationChain2, iptables.Filter)
	if err != nil {
		return fmt.Errorf("failed to create FILTER isolation chain: %v", err)
	}
	defer func() {
		if retErr != nil {
			if err := iptable.RemoveExistingChain(IsolationChain2, iptables.Filter); err != nil {
				log.G(context.TODO()).Warnf("failed on removing iptables FILTER chain %s on cleanup: %v", IsolationChain2, err)
			}
		}
	}()

	if err := addNATJumpRules(version, !config.EnableUserlandProxy, true); err != nil {
		return fmt.Errorf("failed to add jump rules to %s NAT table: %w", version, err)
	}
	defer func() {
		if retErr != nil {
			if err := addNATJumpRules(version, !config.EnableUserlandProxy, false); err != nil {
				log.G(context.TODO()).Warnf("failed on removing jump rules from %s NAT table: %v", version, err)
			}
		}
	}()

	// Make sure the filter-FORWARD chain has rules to accept related packets and
	// jump to the isolation and docker chains. (Re-)insert at the top of the table,
	// in reverse order.
	if err := iptable.EnsureJumpRule("FORWARD", DockerForwardChain); err != nil {
		return err
	}
	if err := iptable.EnsureJumpRule(DockerForwardChain, DockerBridgeChain); err != nil {
		return err
	}
	if err := iptable.EnsureJumpRule(DockerForwardChain, IsolationChain1); err != nil {
		return err
	}
	if err := iptable.EnsureJumpRule(DockerForwardChain, DockerCTChain); err != nil {
		return err
	}

	if err := mirroredWSL2Workaround(config, version); err != nil {
		return err
	}

	// Delete rules that may have been added to the FORWARD chain by moby 28.0.0.
	ipsetName := "docker-ext-bridges-v4"
	if version == iptables.IPv6 {
		ipsetName = "docker-ext-bridges-v6"
	}
	if err := iptable.DeleteJumpRule("FORWARD", DockerChain,
		"-m", "set", "--match-set", ipsetName, "dst"); err != nil {
		log.G(context.TODO()).WithFields(log.Fields{"error": err, "set": ipsetName}).Debug(
			"deleting legacy ipset dest match rule")
	}
	if err := iptable.DeleteJumpRule("FORWARD", IsolationChain1); err != nil {
		return err
	}
	if err := iptable.DeleteJumpRule("FORWARD", "ACCEPT",
		"-m", "set", "--match-set", ipsetName, "dst",
		"-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED",
	); err != nil {
		log.G(context.TODO()).WithFields(log.Fields{"error": err, "set": ipsetName}).Debug(
			"deleting legacy ipset conntrack rule")
	}

	return nil
}

func (n *bridgeNetwork) setupIP4Tables(config *networkConfiguration, i *bridgeInterface) error {
	d := n.driver
	d.Lock()
	driverConfig := d.config
	d.Unlock()

	// Sanity check.
	if !driverConfig.EnableIPTables {
		return errors.New("Cannot program chains, EnableIPTable is disabled")
	}

	maskedAddrv4 := &net.IPNet{
		IP:   i.bridgeIPv4.IP.Mask(i.bridgeIPv4.Mask),
		Mask: i.bridgeIPv4.Mask,
	}
	return n.setupIPTables(iptables.IPv4, maskedAddrv4, config, i)
}

func (n *bridgeNetwork) setupIP6Tables(config *networkConfiguration, i *bridgeInterface) error {
	d := n.driver
	d.Lock()
	driverConfig := d.config
	d.Unlock()

	// Sanity check.
	if !driverConfig.EnableIP6Tables {
		return errors.New("Cannot program chains, EnableIP6Tables is disabled")
	}

	maskedAddrv6 := &net.IPNet{
		IP:   i.bridgeIPv6.IP.Mask(i.bridgeIPv6.Mask),
		Mask: i.bridgeIPv6.Mask,
	}

	return n.setupIPTables(iptables.IPv6, maskedAddrv6, config, i)
}

func (n *bridgeNetwork) setupIPTables(ipVersion iptables.IPVersion, maskedAddr *net.IPNet, config *networkConfiguration, i *bridgeInterface) error {
	var err error

	d := n.driver
	d.Lock()
	driverConfig := d.config
	d.Unlock()

	// Pickup this configuration option from driver
	hairpinMode := !driverConfig.EnableUserlandProxy

	if config.Internal {
		if err = setupInternalNetworkRules(config.BridgeName, maskedAddr, config.EnableICC, true); err != nil {
			return fmt.Errorf("Failed to Setup IP tables: %w", err)
		}
		n.registerIptCleanFunc(func() error {
			return setupInternalNetworkRules(config.BridgeName, maskedAddr, config.EnableICC, false)
		})
	} else {
		if err = setupNonInternalNetworkRules(ipVersion, config, maskedAddr, hairpinMode, true); err != nil {
			return fmt.Errorf("Failed to Setup IP tables: %w", err)
		}
		n.registerIptCleanFunc(func() error {
			return setupNonInternalNetworkRules(ipVersion, config, maskedAddr, hairpinMode, false)
		})

		if err := iptables.AddInterfaceFirewalld(config.BridgeName); err != nil {
			return err
		}
		n.registerIptCleanFunc(func() error {
			if err := iptables.DelInterfaceFirewalld(config.BridgeName); err != nil && !errdefs.IsNotFound(err) {
				return err
			}
			return nil
		})

		err = deleteLegacyFilterRules(ipVersion, config.BridgeName)
		if err != nil {
			return fmt.Errorf("failed to delete legacy rules in filter-FORWARD: %w", err)
		}

		if err := n.setDefaultForwardRule(ipVersion, config.BridgeName); err != nil {
			return err
		}

		ctRule := iptables.Rule{IPVer: ipVersion, Table: iptables.Filter, Chain: DockerCTChain, Args: []string{
			"-o", config.BridgeName,
			"-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED",
			"-j", "ACCEPT",
		}}
		if err := appendOrDelChainRule(ctRule, "bridge ct related", true); err != nil {
			return err
		}
		n.registerIptCleanFunc(func() error {
			return appendOrDelChainRule(ctRule, "bridge ct related", false)
		})
		jumpToDockerRule := iptables.Rule{IPVer: ipVersion, Table: iptables.Filter, Chain: DockerBridgeChain, Args: []string{
			"-o", config.BridgeName,
			"-j", DockerChain,
		}}
		if err := appendOrDelChainRule(jumpToDockerRule, "jump to docker", true); err != nil {
			return err
		}
		n.registerIptCleanFunc(func() error {
			return appendOrDelChainRule(jumpToDockerRule, "jump to docker", false)
		})
	}
	return nil
}

func setICMP(ipv iptables.IPVersion, bridgeName string, enable bool) error {
	icmpProto := "icmp"
	if ipv == iptables.IPv6 {
		icmpProto = "icmpv6"
	}
	icmpRule := iptables.Rule{IPVer: ipv, Table: iptables.Filter, Chain: DockerChain, Args: []string{
		"-o", bridgeName,
		"-p", icmpProto,
		"-j", "ACCEPT",
	}}
	return appendOrDelChainRule(icmpRule, "ICMP", enable)
}

func addNATJumpRules(ipVer iptables.IPVersion, hairpinMode, enable bool) error {
	preroute := iptables.Rule{IPVer: ipVer, Table: iptables.Nat, Chain: "PREROUTING", Args: []string{
		"-m", "addrtype",
		"--dst-type", "LOCAL",
		"-j", DockerChain,
	}}
	if enable {
		if err := preroute.Append(); err != nil {
			return fmt.Errorf("failed to append jump rules to nat-PREROUTING: %s", err)
		}
	} else {
		if err := preroute.Delete(); err != nil {
			return fmt.Errorf("failed to remove jump rules from nat-PREROUTING: %s", err)
		}
	}

	output := iptables.Rule{IPVer: ipVer, Table: iptables.Nat, Chain: "OUTPUT", Args: []string{
		"-m", "addrtype",
		"--dst-type", "LOCAL",
		"-j", DockerChain,
	}}
	if !hairpinMode {
		output.Args = append(output.Args, "!", "--dst", loopbackAddress(ipVer))
	}
	if enable {
		if err := output.Append(); err != nil {
			return fmt.Errorf("failed to append jump rules to nat-OUTPUT: %s", err)
		}
	} else {
		if err := output.Delete(); err != nil {
			return fmt.Errorf("failed to remove jump rules from nat-OUTPUT: %s", err)
		}
	}

	return nil
}

// deleteLegacyFilterRules removes the legacy per-bridge rules from the filter-FORWARD
// chain. This is required for users upgrading the Engine to v28.0.
// TODO(aker): drop this function once Mirantis latest LTS is v28.0 (or higher).
func deleteLegacyFilterRules(ipVer iptables.IPVersion, bridgeName string) error {
	iptable := iptables.GetIptable(ipVer)
	// Delete legacy per-bridge jump to the DOCKER chain from the FORWARD chain, if it exists.
	// These rules have been replaced by an ipset-matching rule.
	link := []string{
		"-o", bridgeName,
		"-j", DockerChain,
	}
	if iptable.Exists(iptables.Filter, "FORWARD", link...) {
		del := append([]string{string(iptables.Delete), "FORWARD"}, link...)
		if output, err := iptable.Raw(del...); err != nil {
			return err
		} else if len(output) != 0 {
			return fmt.Errorf("could not delete linking rule from %s-%s: %s", iptables.Filter, DockerChain, output)
		}
	}

	// Delete legacy per-bridge related/established rule if it exists. These rules
	// have been replaced by an ipset-matching rule.
	establish := []string{
		"-o", bridgeName,
		"-m", "conntrack",
		"--ctstate", "RELATED,ESTABLISHED",
		"-j", "ACCEPT",
	}
	if iptable.Exists(iptables.Filter, "FORWARD", establish...) {
		del := append([]string{string(iptables.Delete), "FORWARD"}, establish...)
		if output, err := iptable.Raw(del...); err != nil {
			return err
		} else if len(output) != 0 {
			return fmt.Errorf("could not delete establish rule from %s-%s: %s", iptables.Filter, DockerChain, output)
		}
	}

	return nil
}

// loopbackAddress returns the loopback address for the given IP version.
func loopbackAddress(version iptables.IPVersion) string {
	switch version {
	case iptables.IPv4, "":
		// IPv4 (default for backward-compatibility)
		return "127.0.0.0/8"
	case iptables.IPv6:
		return "::1/128"
	default:
		panic("unknown IP version: " + version)
	}
}

func (n *bridgeNetwork) setDefaultForwardRule(
	ipVersion iptables.IPVersion,
	bridgeName string,
) error {
	// Normally, DROP anything that hasn't been ACCEPTed by a per-port/protocol
	// rule. This prevents direct access to un-mapped ports from remote hosts
	// that can route directly to the container's address (by setting up a
	// route via the host's address).
	action := "DROP"
	if n.gwMode(ipVersion).unprotected() {
		// If the user really wants to allow all access from the wider network,
		// explicitly ACCEPT anything so that the filter-FORWARD chain's
		// default policy can't interfere.
		action = "ACCEPT"
	}

	rule := iptables.Rule{IPVer: ipVersion, Table: iptables.Filter, Chain: DockerChain, Args: []string{
		"!", "-i", bridgeName,
		"-o", bridgeName,
		"-j", action,
	}}

	// Append to the filter table's DOCKER chain (the default rule must follow
	// per-port ACCEPT rules, which will be inserted at the top of the chain).
	if err := appendOrDelChainRule(rule, "DEFAULT FWD", true); err != nil {
		return fmt.Errorf("failed to add default-drop rule: %w", err)
	}
	n.registerIptCleanFunc(func() error {
		return appendOrDelChainRule(rule, "DEFAULT FWD", false)
	})
	return nil
}

func setupNonInternalNetworkRules(ipVer iptables.IPVersion, config *networkConfiguration, addr *net.IPNet, hairpin, enable bool) error {
	hostIP := config.HostIPv4
	nat := !config.GwModeIPv4.routed()
	if ipVer == iptables.IPv6 {
		hostIP = config.HostIPv6
		nat = !config.GwModeIPv6.routed()
	}

	var natArgs, hpNatArgs []string
	if hostIP != nil {
		// The user wants IPv4/IPv6 SNAT with the given address.
		hostAddr := hostIP.String()
		natArgs = []string{"-s", addr.String(), "!", "-o", config.BridgeName, "-j", "SNAT", "--to-source", hostAddr}
		hpNatArgs = []string{"-m", "addrtype", "--src-type", "LOCAL", "-o", config.BridgeName, "-j", "SNAT", "--to-source", hostAddr}
	} else {
		// Use MASQUERADE, which picks the src-ip based on next-hop from the route table
		natArgs = []string{"-s", addr.String(), "!", "-o", config.BridgeName, "-j", "MASQUERADE"}
		hpNatArgs = []string{"-m", "addrtype", "--src-type", "LOCAL", "-o", config.BridgeName, "-j", "MASQUERADE"}
	}
	natRule := iptables.Rule{IPVer: ipVer, Table: iptables.Nat, Chain: "POSTROUTING", Args: natArgs}
	hpNatRule := iptables.Rule{IPVer: ipVer, Table: iptables.Nat, Chain: "POSTROUTING", Args: hpNatArgs}

	// Set NAT.
	if config.EnableIPMasquerade {
		if nat {
			if err := programChainRule(natRule, "NAT", enable); err != nil {
				return err
			}
		}
		// If the userland proxy is running (!hairpin), skip DNAT for packets originating from
		// this new network. Then, the proxy can pick up the packet from the host address the dest
		// port is published to. Otherwise, if the packet is DNAT'd, it's forwarded straight to the
		// target network, and will be dropped by network isolation rules if it didn't originate in
		// the same bridge network. (So, with the proxy enabled, this skip allows a container in one
		// network to reach a port published by a container in another bridge network.)
		//
		// If the userland proxy is disabled, don't skip, so packets will be DNAT'd. That will
		// enable access to ports published by containers in the same network. But, the INC rules
		// will block access to that published port from containers in other networks. (However,
		// users may add a rule to DOCKER-USER to work around the INC rules if needed.)
		if !hairpin {
			skipDNAT := iptables.Rule{IPVer: ipVer, Table: iptables.Nat, Chain: DockerChain, Args: []string{
				"-i", config.BridgeName,
				"-j", "RETURN",
			}}
			if err := programChainRule(skipDNAT, "SKIP DNAT", enable); err != nil {
				return err
			}
		}
	}

	// In hairpin mode, masquerade traffic from localhost. If hairpin is disabled or if we're tearing down
	// that bridge, make sure the iptables rule isn't lying around.
	if err := programChainRule(hpNatRule, "MASQ LOCAL HOST", enable && hairpin); err != nil {
		return err
	}

	// Set Inter Container Communication.
	if err := setIcc(ipVer, config.BridgeName, config.EnableICC, false, enable); err != nil {
		return err
	}

	// Allow ICMP in routed mode.
	if !nat {
		if err := setICMP(ipVer, config.BridgeName, enable); err != nil {
			return err
		}
	}

	// Handle outgoing packets. This rule was previously added unconditionally
	// to ACCEPT packets that weren't ICC - an extra rule was needed to enable
	// ICC if needed. Those rules are now combined. So, outRuleNoICC is only
	// needed for ICC=false, along with the DROP rule for ICC added by setIcc.
	outRuleNoICC := iptables.Rule{IPVer: ipVer, Table: iptables.Filter, Chain: DockerForwardChain, Args: []string{
		"-i", config.BridgeName,
		"!", "-o", config.BridgeName,
		"-j", "ACCEPT",
	}}
	// If there's a version of outRuleNoICC in the FORWARD chain, created by moby 28.0.0 or older, delete it.
	if enable {
		if err := outRuleNoICC.WithChain("FORWARD").Delete(); err != nil {
			return fmt.Errorf("deleting FORWARD chain outRuleNoICC: %w", err)
		}
	}
	if config.EnableICC {
		// Accept outgoing traffic to anywhere, including other containers on this bridge.
		outRuleICC := iptables.Rule{IPVer: ipVer, Table: iptables.Filter, Chain: DockerForwardChain, Args: []string{
			"-i", config.BridgeName,
			"-j", "ACCEPT",
		}}
		if err := appendOrDelChainRule(outRuleICC, "ACCEPT OUTGOING", enable); err != nil {
			return err
		}
		// If there's a version of outRuleICC in the FORWARD chain, created by moby 28.0.0 or older, delete it.
		if enable {
			if err := outRuleICC.WithChain("FORWARD").Delete(); err != nil {
				return fmt.Errorf("deleting FORWARD chain outRuleICC: %w", err)
			}
		}
	} else {
		// Accept outgoing traffic to anywhere, apart from other containers on this bridge.
		// setIcc added a DROP rule for ICC traffic.
		if err := appendOrDelChainRule(outRuleNoICC, "ACCEPT NON_ICC OUTGOING", enable); err != nil {
			return err
		}
	}

	return nil
}

func programChainRule(rule iptables.Rule, ruleDescr string, insert bool) error {
	operation := "disable"
	fn := rule.Delete
	if insert {
		operation = "enable"
		fn = rule.Insert
	}
	if err := fn(); err != nil {
		return fmt.Errorf("Unable to %s %s rule: %w", operation, ruleDescr, err)
	}
	return nil
}

func appendOrDelChainRule(rule iptables.Rule, ruleDescr string, append bool) error {
	operation := "disable"
	fn := rule.Delete
	if append {
		operation = "enable"
		fn = rule.Append
	}
	if err := fn(); err != nil {
		return fmt.Errorf("Unable to %s %s rule: %w", operation, ruleDescr, err)
	}
	return nil
}

func setIcc(version iptables.IPVersion, bridgeIface string, iccEnable, internal, insert bool) error {
	args := []string{"-i", bridgeIface, "-o", bridgeIface, "-j"}
	acceptRule := iptables.Rule{IPVer: version, Table: iptables.Filter, Chain: DockerForwardChain, Args: append(args, "ACCEPT")}
	dropRule := iptables.Rule{IPVer: version, Table: iptables.Filter, Chain: DockerForwardChain, Args: append(args, "DROP")}

	// The accept rule is no longer required for a bridge with external connectivity, because
	// ICC traffic is allowed by the outgoing-packets rule created by setupIptablesInternal.
	// The accept rule is still required for a --internal network because it has no outgoing
	// rule. If insert and the rule is not required, an ACCEPT rule for an external network
	// may have been left behind by an older version of the daemon so, delete it.
	if insert && iccEnable && internal {
		if err := acceptRule.Append(); err != nil {
			return fmt.Errorf("Unable to allow intercontainer communication: %w", err)
		}
	} else {
		if err := acceptRule.Delete(); err != nil {
			log.G(context.TODO()).WithError(err).Warn("Failed to delete legacy ICC accept rule")
		}
	}

	if insert && !iccEnable {
		if err := dropRule.Append(); err != nil {
			return fmt.Errorf("Unable to prevent intercontainer communication: %w", err)
		}
	} else {
		if err := dropRule.Delete(); err != nil {
			log.G(context.TODO()).WithError(err).Warn("Failed to delete ICC drop rule")
		}
	}

	// Delete rules that may have been inserted into the FORWARD chain by moby 28.0.0 or older.
	if insert {
		if err := acceptRule.WithChain("FORWARD").Delete(); err != nil {
			return fmt.Errorf("deleting FORWARD chain accept rule: %w", err)
		}
		if err := dropRule.WithChain("FORWARD").Delete(); err != nil {
			return fmt.Errorf("deleting FORWARD chain drop rule: %w", err)
		}
	}
	return nil
}

// Control Inter-Network Communication.
// Install rules only if they aren't present, remove only if they are.
// If this method returns an error, it doesn't roll back any rules it has added.
// No error is returned if rules cannot be removed (errors are just logged).
func setINC(version iptables.IPVersion, iface string, gwm gwMode, enable bool) (retErr error) {
	iptable := iptables.GetIptable(version)
	actionI, actionA := iptables.Insert, iptables.Append
	actionMsg := "add"
	if !enable {
		actionI, actionA = iptables.Delete, iptables.Delete
		actionMsg = "remove"
	}

	if gwm.routed() {
		// Anything is allowed into a routed network at this stage, so RETURN. Port
		// filtering rules in the DOCKER chain will drop anything that's not destined
		// for an open port.
		if err := iptable.ProgramRule(iptables.Filter, IsolationChain1, actionI, []string{
			"-o", iface,
			"-j", "RETURN",
		}); err != nil {
			log.G(context.TODO()).WithError(err).Warnf("Failed to %s inter-network communication rule", actionMsg)
			if enable {
				return fmt.Errorf("%s inter-network communication rule: %w", actionMsg, err)
			}
		}

		// Allow responses from the routed network into whichever network made the request.
		if err := iptable.ProgramRule(iptables.Filter, IsolationChain1, actionI, []string{
			"-i", iface,
			"-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED",
			"-j", "ACCEPT",
		}); err != nil {
			log.G(context.TODO()).WithError(err).Warnf("Failed to %s inter-network communication rule", actionMsg)
			if enable {
				return fmt.Errorf("%s inter-network communication rule: %w", actionMsg, err)
			}
		}
	}

	if err := iptable.ProgramRule(iptables.Filter, IsolationChain1, actionA, []string{
		"-i", iface,
		"!", "-o", iface,
		"-j", IsolationChain2,
	}); err != nil {
		log.G(context.TODO()).WithError(err).Warnf("Failed to %s inter-network communication rule", actionMsg)
		if enable {
			return fmt.Errorf("%s inter-network communication rule: %w", actionMsg, err)
		}
	}

	if err := iptable.ProgramRule(iptables.Filter, IsolationChain2, actionI, []string{
		"-o", iface,
		"-j", "DROP",
	}); err != nil {
		log.G(context.TODO()).WithError(err).Warnf("Failed to %s inter-network communication rule", actionMsg)
		if enable {
			return fmt.Errorf("%s inter-network communication rule: %w", actionMsg, err)
		}
	}

	return nil
}

// Obsolete chain from previous docker versions
const oldIsolationChain = "DOCKER-ISOLATION"

func removeIPChains(version iptables.IPVersion) {
	ipt := iptables.GetIptable(version)

	// Remove obsolete rules from default chains
	ipt.ProgramRule(iptables.Filter, "FORWARD", iptables.Delete, []string{"-j", oldIsolationChain})

	// Remove chains
	for _, chainInfo := range []iptables.ChainInfo{
		{Name: DockerChain, Table: iptables.Nat, IPVersion: version},
		{Name: DockerChain, Table: iptables.Filter, IPVersion: version},
		{Name: DockerForwardChain, Table: iptables.Filter, IPVersion: version},
		{Name: DockerBridgeChain, Table: iptables.Filter, IPVersion: version},
		{Name: DockerCTChain, Table: iptables.Filter, IPVersion: version},
		{Name: IsolationChain1, Table: iptables.Filter, IPVersion: version},
		{Name: IsolationChain2, Table: iptables.Filter, IPVersion: version},
		{Name: oldIsolationChain, Table: iptables.Filter, IPVersion: version},
	} {
		if err := chainInfo.Remove(); err != nil {
			log.G(context.TODO()).Warnf("Failed to remove existing iptables entries in table %s chain %s : %v", chainInfo.Table, chainInfo.Name, err)
		}
	}
}

func setupInternalNetworkRules(bridgeIface string, addr *net.IPNet, icc, insert bool) error {
	var version iptables.IPVersion
	var inDropRule, outDropRule iptables.Rule

	// Either add or remove the interface from the firewalld zone, if firewalld is running.
	if insert {
		if err := iptables.AddInterfaceFirewalld(bridgeIface); err != nil {
			return err
		}
	} else {
		if err := iptables.DelInterfaceFirewalld(bridgeIface); err != nil && !errdefs.IsNotFound(err) {
			return err
		}
	}

	if addr.IP.To4() != nil {
		version = iptables.IPv4
		inDropRule = iptables.Rule{
			IPVer: version,
			Table: iptables.Filter,
			Chain: IsolationChain1,
			Args:  []string{"-i", bridgeIface, "!", "-d", addr.String(), "-j", "DROP"},
		}
		outDropRule = iptables.Rule{
			IPVer: version,
			Table: iptables.Filter,
			Chain: IsolationChain1,
			Args:  []string{"-o", bridgeIface, "!", "-s", addr.String(), "-j", "DROP"},
		}
	} else {
		version = iptables.IPv6
		inDropRule = iptables.Rule{
			IPVer: version,
			Table: iptables.Filter,
			Chain: IsolationChain1,
			Args:  []string{"-i", bridgeIface, "!", "-o", bridgeIface, "!", "-d", addr.String(), "-j", "DROP"},
		}
		outDropRule = iptables.Rule{
			IPVer: version,
			Table: iptables.Filter,
			Chain: IsolationChain1,
			Args:  []string{"!", "-i", bridgeIface, "-o", bridgeIface, "!", "-s", addr.String(), "-j", "DROP"},
		}
	}

	if err := programChainRule(inDropRule, "DROP INCOMING", insert); err != nil {
		return err
	}
	if err := programChainRule(outDropRule, "DROP OUTGOING", insert); err != nil {
		return err
	}

	// Set Inter Container Communication.
	return setIcc(version, bridgeIface, icc, true, insert)
}

// clearConntrackEntries flushes conntrack entries matching endpoint IP address
// or matching one of the exposed UDP port.
// In the first case, this could happen if packets were received by the host
// between userland proxy startup and iptables setup.
// In the latter case, this could happen if packets were received whereas there
// were nowhere to route them, as netfilter creates entries in such case.
// This is required because iptables NAT rules are evaluated by netfilter only
// when creating a new conntrack entry. When Docker latter adds NAT rules,
// netfilter ignore them for any packet matching a pre-existing conntrack entry.
// As such, we need to flush all those conntrack entries to make sure NAT rules
// are correctly applied to all packets.
// See: #8795, #44688 & #44742.
func clearConntrackEntries(nlh nlwrap.Handle, ep *bridgeEndpoint) {
	var ipv4List []net.IP
	var ipv6List []net.IP
	var udpPorts []uint16

	if ep.addr != nil {
		ipv4List = append(ipv4List, ep.addr.IP)
	}
	if ep.addrv6 != nil {
		ipv6List = append(ipv6List, ep.addrv6.IP)
	}
	for _, pb := range ep.portMapping {
		if pb.Proto == types.UDP {
			udpPorts = append(udpPorts, pb.HostPort)
		}
	}

	iptables.DeleteConntrackEntries(nlh, ipv4List, ipv6List)
	iptables.DeleteConntrackEntriesByPort(nlh, types.UDP, udpPorts)
}

// mirroredWSL2Workaround adds or removes an IPv4 NAT rule, depending on whether
// docker's host Linux appears to be a guest running under WSL2 in with mirrored
// mode networking.
// https://learn.microsoft.com/en-us/windows/wsl/networking#mirrored-mode-networking
//
// Without mirrored mode networking, or for a packet sent from Linux, packets
// sent to 127.0.0.1 are processed as outgoing - they hit the nat-OUTPUT chain,
// which does not jump to the nat-DOCKER chain because the rule has an exception
// for "-d 127.0.0.0/8". The default action on the nat-OUTPUT chain is ACCEPT (by
// default), so the packet is delivered to 127.0.0.1 on lo, where docker-proxy
// picks it up and acts as a man-in-the-middle; it receives the packet and
// re-sends it to the container (or acks a SYN and sets up a second TCP
// connection to the container). So, the container sees packets arrive with a
// source address belonging to the network's bridge, and it is able to reply to
// that address.
//
// In WSL2's mirrored networking mode, Linux has a loopback0 device as well as lo
// (which owns 127.0.0.1 as normal). Packets sent to 127.0.0.1 from Windows to a
// server listening on Linux's 127.0.0.1 are delivered via loopback0, and
// processed as packets arriving from outside the Linux host (which they are).
//
// So, these packets hit the nat-PREROUTING chain instead of nat-OUTPUT. It would
// normally be impossible for a packet ->127.0.0.1 to arrive from outside the
// host, so the nat-PREROUTING jump to nat-DOCKER has no exception for it. The
// packet is processed by a per-bridge DNAT rule in that chain, so it is
// delivered directly to the container (not via docker-proxy) with source address
// 127.0.0.1, so the container can't respond.
//
// DNAT is normally skipped by RETURN rules in the nat-DOCKER chain for packets
// arriving from any other bridge network. Similarly, this function adds (or
// removes) a rule to RETURN early for packets delivered via loopback0 with
// destination 127.0.0.0/8.
func mirroredWSL2Workaround(config configuration, ipv iptables.IPVersion) error {
	// WSL2 does not (currently) support Windows<->Linux communication via ::1.
	if ipv != iptables.IPv4 {
		return nil
	}
	return programChainRule(mirroredWSL2Rule(), "WSL2 loopback", insertMirroredWSL2Rule(config))
}

// insertMirroredWSL2Rule returns true if the NAT rule for mirrored WSL2 workaround
// is required. It is required if:
//   - the userland proxy is running. If not, there's nothing on the host to catch
//     the packet, so the loopback0 rule as wouldn't be useful. However, without
//     the workaround, with improvements in WSL2 v2.3.11, and without userland proxy
//     running - no workaround is needed, the normal DNAT/masquerading works.
//   - and, the host Linux appears to be running under Windows WSL2 with mirrored
//     mode networking.
func insertMirroredWSL2Rule(config configuration) bool {
	if !config.EnableUserlandProxy || config.UserlandProxyPath == "" {
		return false
	}
	return isRunningUnderWSL2MirroredMode()
}

// isRunningUnderWSL2MirroredMode returns true if the host Linux appears to be
// running under Windows WSL2 with mirrored mode networking. If a loopback0
// device exists, and there's an executable at /usr/bin/wslinfo, infer that
// this is WSL2 with mirrored networking. ("wslinfo --networking-mode" reports
// "mirrored", but applying the workaround for WSL2's loopback device when it's
// not needed is low risk, compared with executing wslinfo with dockerd's
// elevated permissions.)
func isRunningUnderWSL2MirroredMode() bool {
	if _, err := nlwrap.LinkByName("loopback0"); err != nil {
		if !errors.As(err, &netlink.LinkNotFoundError{}) {
			log.G(context.TODO()).WithError(err).Warn("Failed to check for WSL interface")
		}
		return false
	}
	stat, err := os.Stat(wslinfoPath)
	if err != nil {
		return false
	}
	return stat.Mode().IsRegular() && (stat.Mode().Perm()&0o111) != 0
}

func mirroredWSL2Rule() iptables.Rule {
	return iptables.Rule{
		IPVer: iptables.IPv4,
		Table: iptables.Nat,
		Chain: DockerChain,
		Args:  []string{"-i", "loopback0", "-d", "127.0.0.0/8", "-j", "RETURN"},
	}
}
