package com.blhsing.deskferry.home;

import org.junit.Test;

import static org.junit.Assert.assertEquals;

public class ScreenViewerActivityTest {
    @Test
    public void screenReconnectDelayGrowsToThirtySeconds() {
        long[] want = {2_000, 4_000, 8_000, 16_000, 30_000, 30_000};
        for (int attempt = 0; attempt < want.length; attempt++) {
            assertEquals(want[attempt], ScreenViewerActivity.screenReconnectDelayMs(attempt));
        }
    }
}
