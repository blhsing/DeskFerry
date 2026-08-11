package com.blhsing.deskferry.home;

import org.junit.Test;

import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

public class ScreenViewerActivityTest {
    @Test
    public void screenSessionRecoveryIsBounded() {
        assertTrue(ScreenViewerActivity.canRetryScreenSession(1));
        assertTrue(ScreenViewerActivity.canRetryScreenSession(2));
        assertFalse(ScreenViewerActivity.canRetryScreenSession(3));
    }
}
