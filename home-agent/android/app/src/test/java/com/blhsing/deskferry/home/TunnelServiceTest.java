package com.blhsing.deskferry.home;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import org.junit.Test;

public class TunnelServiceTest {
    @Test
    public void resumeAttemptDoesNotConsumeRecoveryWindow() {
        assertEquals(20_000L, TunnelService.resumeAttemptWaitMillis(300_000L));
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
}
