package com.blhsing.deskferry.home;

import static org.junit.Assert.assertEquals;

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
}
