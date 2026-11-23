package com.blhsing.deskferry.home;

import org.junit.Test;

import java.io.ByteArrayInputStream;
import java.io.ByteArrayOutputStream;
import java.io.DataInputStream;
import java.io.IOException;
import java.nio.ByteBuffer;
import java.nio.charset.StandardCharsets;
import java.security.KeyPair;
import java.security.interfaces.ECPublicKey;
import java.util.Arrays;
import java.util.List;

import javax.crypto.KeyAgreement;

import static org.junit.Assert.assertArrayEquals;
import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNull;
import static org.junit.Assert.assertTrue;
import static org.junit.Assert.fail;

public class LiveScreenProtocolTest {
    private static byte[] hex(String value) {
        byte[] out = new byte[value.length() / 2];
        for (int i = 0; i < out.length; i++) out[i] = (byte) Integer.parseInt(value.substring(2 * i, 2 * i + 2), 16);
        return out;
    }

    @Test
    public void hkdfMatchesRfc5869TestCase1() {
        byte[] okm = ScreenKeys.hkdf(hex("0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b"),
                hex("000102030405060708090a0b0c"), hex("f0f1f2f3f4f5f6f7f8f9"), 42);
        assertArrayEquals(hex("3cb25f25faacd57a90434f64d0362f2a2d2d0a90cf1a5a4c5db02d56ecc4c5bf34007208d5b887185865"), okm);
    }

    @Test
    public void screenKeyMatchesIndependentVector() {
        // Computed independently with Python hmac/hashlib (RFC 5869).
        // Same vector as TestDeriveScreenKeyVector in the Go implementation.
        assertEquals("Z2eEzs6T6rqmtt2ZMsMIPsDWsknsfEUvRLfwZxkTmBU", ScreenKeys.deriveScreenKey("room", "secret"));
        assertEquals("Z2eEzs6T6rqmtt2ZMsMIPsDWsknsfEUvRLfwZxkTmBU", ScreenKeys.deriveScreenKey("  ROOM ", "secret"));
        assertEquals(ScreenKeys.fromProof(ScreenKeys.roomProof("room", "secret")), ScreenKeys.deriveScreenKey("room", "secret"));
        assertEquals("", ScreenKeys.deriveScreenKey("room", ""));
        assertEquals(32, ScreenKeys.decodeScreenKey(ScreenKeys.deriveScreenKey("room", "secret")).length);
        assertNull(ScreenKeys.decodeScreenKey("not a key"));
    }

    @Test
    public void screenKeyForRelayUsesTheRoomName() {
        assertEquals(ScreenKeys.deriveScreenKey("workdesk", "secret"),
                ScreenKeys.deriveScreenKeyForRelay("https://relay.example/relay/workdesk", "secret"));
    }

    @Test
    public void uncompressedPointRoundTrip() throws Exception {
        KeyPair pair = LiveScreenProtocol.generateKeyPair();
        ECPublicKey key = (ECPublicKey) pair.getPublic();
        byte[] encoded = LiveScreenProtocol.encodePoint(key);
        assertEquals(65, encoded.length);
        assertEquals(0x04, encoded[0]);
        ECPublicKey decoded = LiveScreenProtocol.decodePoint(encoded, key.getParams());
        assertEquals(key.getW(), decoded.getW());
        assertArrayEquals(encoded, LiveScreenProtocol.encodePoint(decoded));
        byte[] bad = encoded.clone();
        bad[64] ^= 1;
        try {
            LiveScreenProtocol.decodePoint(bad, key.getParams());
            fail("off-curve point accepted");
        } catch (IOException expected) {
        }
    }

    @Test
    public void handshakeAgreesWithAServerAndDetectsWrongPassword() throws Exception {
        byte[] psk = ScreenKeys.decodeScreenKey(ScreenKeys.deriveScreenKey("room", "secret"));
        LiveScreenProtocol.ClientHandshake client = new LiveScreenProtocol.ClientHandshake(psk);
        assertEquals(97, client.hello.length);

        // Server side, following docs/screen-live-protocol.md.
        KeyPair server = LiveScreenProtocol.generateKeyPair();
        byte[] pubNonce = LiveScreenProtocol.concat(LiveScreenProtocol.encodePoint((ECPublicKey) server.getPublic()),
                LiveScreenProtocol.randomBytes(32));
        byte[] transcript = LiveScreenProtocol.transcript(client.hello, pubNonce);
        byte[] serverHello = LiveScreenProtocol.concat(pubNonce,
                LiveScreenProtocol.handshakeMac(psk, "server finished\u0000", transcript));
        LiveScreenProtocol.SessionKeys keys = client.finish(serverHello);

        KeyAgreement agreement = KeyAgreement.getInstance("ECDH");
        agreement.init(server.getPrivate());
        agreement.doPhase(LiveScreenProtocol.decodePoint(Arrays.copyOf(client.hello, 65),
                ((ECPublicKey) server.getPublic()).getParams()), true);
        byte[] prk = ScreenKeys.hkdfExtract(transcript, LiveScreenProtocol.concat(agreement.generateSecret(), psk));
        assertArrayEquals(ScreenKeys.hkdfExpand(prk, "DeskFerry c2s".getBytes(StandardCharsets.UTF_8), 32), keys.c2s);
        assertArrayEquals(ScreenKeys.hkdfExpand(prk, "DeskFerry s2c".getBytes(StandardCharsets.UTF_8), 32), keys.s2c);
        assertArrayEquals(LiveScreenProtocol.handshakeMac(psk, "client finished\u0000", transcript), keys.clientFinish);

        byte[] otherPsk = ScreenKeys.decodeScreenKey(ScreenKeys.deriveScreenKey("room", "other"));
        try {
            new LiveScreenProtocol.ClientHandshake(otherPsk).finish(serverHello);
            fail("wrong password accepted");
        } catch (LiveScreenProtocol.PasswordMismatchException expected) {
        }
    }

    @Test
    public void recordNonceLayout() {
        byte[] nonce = LiveScreenProtocol.recordNonce(0x0102030405060708L);
        assertArrayEquals(new byte[]{0, 0, 0, 0, 1, 2, 3, 4, 5, 6, 7, 8}, nonce);
        assertArrayEquals(new byte[12], LiveScreenProtocol.recordNonce(0));
    }

    @Test
    public void gcmRecordRoundTripWithSequences() throws Exception {
        byte[] key = new byte[32];
        Arrays.fill(key, (byte) 7);
        LiveScreenProtocol.RecordCipher sender = new LiveScreenProtocol.RecordCipher(key);
        LiveScreenProtocol.RecordCipher receiver = new LiveScreenProtocol.RecordCipher(key);
        ByteArrayOutputStream wire = new ByteArrayOutputStream();
        byte[] first = sender.seal(LiveScreenProtocol.REC_ACK, LiveScreenProtocol.encodeSeq(42));
        assertEquals(4 + 1 + 8 + 16, first.length);
        assertEquals(first.length - 4, ByteBuffer.wrap(first).getInt());
        wire.write(first);
        wire.write(sender.seal(LiveScreenProtocol.REC_JSON, "{\"cmd\":\"status\"}".getBytes(StandardCharsets.UTF_8)));
        assertEquals(2, sender.sequence());

        // Record 0 uses nonce 0, record 1 uses nonce 1: verify against a raw AES-GCM cipher.
        javax.crypto.Cipher raw = javax.crypto.Cipher.getInstance("AES/GCM/NoPadding");
        raw.init(javax.crypto.Cipher.DECRYPT_MODE, new javax.crypto.spec.SecretKeySpec(key, "AES"),
                new javax.crypto.spec.GCMParameterSpec(128, LiveScreenProtocol.recordNonce(0)));
        byte[] plain = raw.doFinal(Arrays.copyOfRange(first, 4, first.length));
        assertEquals(LiveScreenProtocol.REC_ACK, plain[0]);
        assertEquals(42, LiveScreenProtocol.getLong(plain, 1));

        DataInputStream in = new DataInputStream(new ByteArrayInputStream(wire.toByteArray()));
        byte[] one = receiver.read(in);
        assertEquals(LiveScreenProtocol.REC_ACK, one[0]);
        byte[] two = receiver.read(in);
        assertEquals(LiveScreenProtocol.REC_JSON, two[0]);
        assertEquals("{\"cmd\":\"status\"}", new String(two, 1, two.length - 1, StandardCharsets.UTF_8));

        // Replaying a record fails authentication because the sequence moved on.
        LiveScreenProtocol.RecordCipher replay = new LiveScreenProtocol.RecordCipher(key);
        replay.open(Arrays.copyOfRange(first, 4, first.length));
        try {
            replay.open(Arrays.copyOfRange(first, 4, first.length));
            fail("replayed record accepted");
        } catch (IOException expected) {
        }
    }

    @Test
    public void parsesFrameStartAndEnd() throws Exception {
        ByteBuffer start = ByteBuffer.allocate(18);
        start.put((byte) 0x10).putLong(9).putInt(1920).putInt(1080).put((byte) 1);
        LiveScreenProtocol.FrameStart frameStart = LiveScreenProtocol.parseFrameStart(start.array(), 1, 17);
        assertEquals(9, frameStart.seq);
        assertEquals(1920, frameStart.width);
        assertEquals(1080, frameStart.height);
        assertTrue(frameStart.keyframe());

        ByteBuffer end = ByteBuffer.allocate(9);
        end.putLong(9).put((byte) 3);
        LiveScreenProtocol.FrameEnd frameEnd = LiveScreenProtocol.parseFrameEnd(end.array(), 0, 9);
        assertEquals(9, frameEnd.seq);
        assertTrue(frameEnd.lossless());
        assertTrue(frameEnd.settled());

        ByteBuffer zero = ByteBuffer.allocate(17);
        zero.putLong(1).putInt(0).putInt(10).put((byte) 0);
        try {
            LiveScreenProtocol.parseFrameStart(zero.array(), 0, 17);
            fail("zero width accepted");
        } catch (IOException expected) {
        }
    }

    @Test
    public void parsesRects() throws Exception {
        byte[] png = new byte[]{(byte) 0x89, 'P', 'N', 'G', 1, 2};
        ByteBuffer rects = ByteBuffer.allocate(2 + 13 + 4 + 13 + png.length);
        rects.putShort((short) 2);
        rects.putShort((short) 10).putShort((short) 20).putShort((short) 64).putShort((short) 32).put((byte) 0).putInt(4);
        rects.put(new byte[]{(byte) 0x12, (byte) 0x34, (byte) 0x56, (byte) 0xFF});
        rects.putShort((short) 65000).putShort((short) 1).putShort((short) 3).putShort((short) 4).put((byte) 1).putInt(png.length);
        rects.put(png);
        List<LiveScreenProtocol.Rect> parsed = LiveScreenProtocol.parseRects(rects.array(), 0, rects.capacity());
        assertEquals(2, parsed.size());
        LiveScreenProtocol.Rect solid = parsed.get(0);
        assertEquals(10, solid.x);
        assertEquals(20, solid.y);
        assertEquals(64, solid.w);
        assertEquals(32, solid.h);
        assertEquals(LiveScreenProtocol.ENC_SOLID, solid.enc);
        assertEquals(0xFF123456, solid.solidArgb());
        LiveScreenProtocol.Rect image = parsed.get(1);
        assertEquals(65000, image.x);
        assertEquals(LiveScreenProtocol.ENC_PNG, image.enc);
        assertArrayEquals(png, Arrays.copyOfRange(image.data, image.dataOffset, image.dataOffset + image.dataLength));

        byte[] truncated = Arrays.copyOf(rects.array(), rects.capacity() - 1);
        try {
            LiveScreenProtocol.parseRects(truncated, 0, truncated.length);
            fail("truncated rects accepted");
        } catch (IOException expected) {
        }
    }

    @Test
    public void parsesCursor() throws Exception {
        byte[] png = new byte[]{1, 2, 3};
        ByteBuffer cursor = ByteBuffer.allocate(21 + png.length);
        cursor.putInt(-5).putInt(700).put((byte) 1).putInt(0xFFFFFFF0).putShort((short) 3).putShort((short) 4).putInt(png.length).put(png);
        LiveScreenProtocol.Cursor parsed = LiveScreenProtocol.parseCursor(cursor.array(), 0, cursor.capacity());
        assertEquals(-5, parsed.x);
        assertEquals(700, parsed.y);
        assertTrue(parsed.visible);
        assertEquals(0xFFFFFFF0L, parsed.shapeId);
        assertEquals(3, parsed.hotX);
        assertEquals(4, parsed.hotY);
        assertArrayEquals(png, parsed.png);

        ByteBuffer unchanged = ByteBuffer.allocate(21);
        unchanged.putInt(1).putInt(2).put((byte) 0).putInt(7).putShort((short) 0).putShort((short) 0).putInt(0);
        LiveScreenProtocol.Cursor same = LiveScreenProtocol.parseCursor(unchanged.array(), 0, 21);
        assertFalse(same.visible);
        assertEquals(0, same.png.length);

        unchanged.putInt(17, 5);
        try {
            LiveScreenProtocol.parseCursor(unchanged.array(), 0, 21);
            fail("bad shape length accepted");
        } catch (IOException expected) {
        }
    }

    @Test
    public void mapsViewPointsToFramebuffer() {
        // Scale 0.5 with the image centred 100 px from the left edge.
        assertArrayEquals(new int[]{0, 0}, LiveScreenProtocol.viewToFramebuffer(100, 0, 0.5f, 100, 0, 1920, 1080));
        assertArrayEquals(new int[]{200, 300}, LiveScreenProtocol.viewToFramebuffer(200, 150, 0.5f, 100, 0, 1920, 1080));
        // Zoomed in 2x and panned.
        assertArrayEquals(new int[]{510, 260}, LiveScreenProtocol.viewToFramebuffer(20, 20, 2f, -1000, -500, 1920, 1080));
        // Points outside the image clamp to its edges.
        assertArrayEquals(new int[]{0, 1079}, LiveScreenProtocol.viewToFramebuffer(50, 5000, 0.5f, 100, 0, 1920, 1080));
        assertArrayEquals(new int[]{1919, 0}, LiveScreenProtocol.viewToFramebuffer(5000, -10, 0.5f, 100, 0, 1920, 1080));
    }

    @Test
    public void mapsAndroidKeysToWindowsVirtualKeys() {
        assertArrayEquals(new int[]{0x08, 0}, ScreenViewerActivity.windowsVirtualKey(android.view.KeyEvent.KEYCODE_DEL));
        assertArrayEquals(new int[]{0x0D, 0}, ScreenViewerActivity.windowsVirtualKey(android.view.KeyEvent.KEYCODE_ENTER));
        assertArrayEquals(new int[]{0x25, 1}, ScreenViewerActivity.windowsVirtualKey(android.view.KeyEvent.KEYCODE_DPAD_LEFT));
        assertArrayEquals(new int[]{0x71, 0}, ScreenViewerActivity.windowsVirtualKey(android.view.KeyEvent.KEYCODE_F2));
        assertNull(ScreenViewerActivity.windowsVirtualKey(android.view.KeyEvent.KEYCODE_A));
    }
}
