package com.blhsing.deskferry.home;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import org.junit.Test;

public class TunnelServiceTest {
    @Test
    public void resumeAttemptDoesNotConsumeRecoveryWindow() {
        assertEquals(8_000L, TunnelService.resumeAttemptWaitMillis(300_000L));
    }

    @Test
    public void resumeAttemptUsesShortRemainingWindow() {
        assertEquals(7_500L, TunnelService.resumeAttemptWaitMillis(7_500L));
    }

    @Test
    public void resumeAttemptNeverUsesNonPositiveWait() {
        assertEquals(1L, TunnelService.resumeAttemptWaitMillis(0L));
    }

    @Test
    public void normalProxyCloseStillReconnects() {
        assertFalse(TunnelService.isLogicalSessionClose(1000, ""));
        assertFalse(TunnelService.isLogicalSessionClose(1000, "closed"));
    }

    @Test
    public void explicitSessionCloseDoesNotReconnect() {
        assertTrue(TunnelService.isLogicalSessionClose(1000, "session closed"));
    }

    @Test
    public void completedOrRejectedResumeIsTerminal() {
        assertTrue(TunnelService.isTerminalResumeClose(1000));
        assertTrue(TunnelService.isTerminalResumeClose(1008));
        assertFalse(TunnelService.isTerminalResumeClose(1012));
        assertFalse(TunnelService.isTerminalResumeClose(1013));
    }

    @Test
    public void idleProtocolV2WorkAgentIsOnline() {
        assertTrue(TunnelService.workAgentOnline(1, 0, 0));
        assertTrue(TunnelService.workAgentOnline(0, 1, 0));
        assertTrue(TunnelService.workAgentOnline(0, 0, 1));
        assertFalse(TunnelService.workAgentOnline(0, 0, 0));
    }

    @Test
    public void roomAuthenticationFailureIsRecognized() {
        assertTrue(TunnelService.isRoomAuthenticationFailure(1008, "room authentication failed"));
        assertFalse(TunnelService.isRoomAuthenticationFailure(1008, "unsupported role"));
        assertFalse(TunnelService.isRoomAuthenticationFailure(1000, "room authentication failed"));
        assertFalse(TunnelService.isRoomAuthenticationFailure(1008, null));
    }
}
