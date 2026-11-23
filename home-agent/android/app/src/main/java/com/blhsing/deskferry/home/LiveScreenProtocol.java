package com.blhsing.deskferry.home;

import java.io.DataInputStream;
import java.io.IOException;
import java.math.BigInteger;
import java.nio.charset.StandardCharsets;
import java.security.KeyFactory;
import java.security.KeyPair;
import java.security.KeyPairGenerator;
import java.security.MessageDigest;
import java.security.PublicKey;
import java.security.SecureRandom;
import java.security.interfaces.ECPublicKey;
import java.security.spec.ECFieldFp;
import java.security.spec.ECGenParameterSpec;
import java.security.spec.ECParameterSpec;
import java.security.spec.ECPoint;
import java.security.spec.ECPublicKeySpec;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;

import javax.crypto.Cipher;
import javax.crypto.KeyAgreement;
import javax.crypto.spec.GCMParameterSpec;
import javax.crypto.spec.SecretKeySpec;

/**
 * Screen protocol 2 primitives for the Android Home viewer: the P-256 + PSK handshake, AES-256-GCM
 * secure records, and the Work-to-client record parsers. See docs/screen-live-protocol.md; all
 * integers are big-endian. Kept free of Android APIs so it can be unit tested on the JVM.
 */
final class LiveScreenProtocol {
    static final int PROTOCOL = 2;

    static final byte REC_FINISH = 0x01;
    static final byte REC_ACK = 0x02;
    static final byte REC_FRAME_START = 0x10;
    static final byte REC_RECTS = 0x11;
    static final byte REC_FRAME_END = 0x12;
    static final byte REC_CURSOR = 0x13;
    static final byte REC_JSON = 0x20;
    static final byte REC_PING = 0x7E;
    static final byte REC_PONG = 0x7F;

    static final int ENC_SOLID = 0;
    static final int ENC_PNG = 1;
    static final int ENC_JPEG = 2;
    /** A JPEG at reduced resolution, drawn stretched to its rect. */
    static final int ENC_JPEG_SCALED = 3;
    static final String FEATURE_SCALED_JPEG = "scaled-jpeg";
    /** One access unit of an H.264 stream: a flags byte, then Annex B NAL units. */
    static final int ENC_H264 = 4;
    static final String FEATURE_H264 = "h264";
    /** Flag of an encoding-4 rect's first data byte: a new H.264 stream starts with this unit. */
    static final int VIDEO_STREAM_START = 1;

    static final int FRAME_FLAG_KEY = 1;
    static final int FRAME_END_LOSSLESS = 1;
    static final int FRAME_END_SETTLED = 2;

    static final int POINT_SIZE = 65;
    static final int NONCE_SIZE = 32;
    static final int CLIENT_HELLO_SIZE = POINT_SIZE + NONCE_SIZE;
    static final int SERVER_HELLO_SIZE = POINT_SIZE + NONCE_SIZE + 32;
    static final int MAX_RECORD_CIPHER = 2 << 20;
    static final int MAX_RECORD_PLAIN = 1 << 20;
    static final int GCM_TAG_BYTES = 16;
    private static final int RECT_HEADER_SIZE = 13;

    private LiveScreenProtocol() {
    }

    /** The Work agent proved a different room password than the stored screen key. */
    static final class PasswordMismatchException extends IOException {
        PasswordMismatchException() {
            super("room password mismatch");
        }
    }

    // ---- P-256 points -------------------------------------------------------------------

    /** Encodes a P-256 public key as the 65-byte uncompressed point 0x04 || X || Y. */
    static byte[] encodePoint(ECPublicKey key) {
        ECPoint point = key.getW();
        byte[] out = new byte[POINT_SIZE];
        out[0] = 0x04;
        writeUnsigned(point.getAffineX(), out, 1, 32);
        writeUnsigned(point.getAffineY(), out, 33, 32);
        return out;
    }

    /** Decodes a 65-byte uncompressed point into a public key on the curve of {@code params}. */
    static ECPublicKey decodePoint(byte[] encoded, ECParameterSpec params) throws IOException {
        if (encoded == null || encoded.length != POINT_SIZE || encoded[0] != 0x04) {
            throw new IOException("invalid P-256 point encoding");
        }
        BigInteger x = new BigInteger(1, Arrays.copyOfRange(encoded, 1, 33));
        BigInteger y = new BigInteger(1, Arrays.copyOfRange(encoded, 33, 65));
        if (!onCurve(params, x, y)) {
            throw new IOException("P-256 point is not on the curve");
        }
        try {
            PublicKey key = KeyFactory.getInstance("EC").generatePublic(new ECPublicKeySpec(new ECPoint(x, y), params));
            return (ECPublicKey) key;
        } catch (Exception ex) {
            throw new IOException("invalid P-256 public key", ex);
        }
    }

    static boolean onCurve(ECParameterSpec params, BigInteger x, BigInteger y) {
        BigInteger p = ((ECFieldFp) params.getCurve().getField()).getP();
        if (x.signum() < 0 || y.signum() < 0 || x.compareTo(p) >= 0 || y.compareTo(p) >= 0) return false;
        BigInteger left = y.multiply(y).mod(p);
        BigInteger right = x.pow(3).add(params.getCurve().getA().multiply(x)).add(params.getCurve().getB()).mod(p);
        return left.equals(right);
    }

    static KeyPair generateKeyPair() {
        try {
            KeyPairGenerator generator = KeyPairGenerator.getInstance("EC");
            generator.initialize(new ECGenParameterSpec("secp256r1"), new SecureRandom());
            return generator.generateKeyPair();
        } catch (Exception ex) {
            throw new IllegalStateException("P-256 is unavailable", ex);
        }
    }

    private static void writeUnsigned(BigInteger value, byte[] out, int offset, int size) {
        byte[] raw = value.toByteArray();
        int start = 0;
        while (start < raw.length - 1 && raw[start] == 0) start++;
        int length = raw.length - start;
        if (length > size) throw new IllegalArgumentException("coordinate is too large");
        System.arraycopy(raw, start, out, offset + size - length, length);
    }

    // ---- Handshake ----------------------------------------------------------------------

    static final class SessionKeys {
        final byte[] c2s;
        final byte[] s2c;
        final byte[] clientFinish;

        SessionKeys(byte[] c2s, byte[] s2c, byte[] clientFinish) {
            this.c2s = c2s;
            this.s2c = s2c;
            this.clientFinish = clientFinish;
        }
    }

    static final class ClientHandshake {
        private final byte[] psk;
        private final KeyPair keyPair;
        final byte[] hello;

        ClientHandshake(byte[] psk) {
            this(psk, generateKeyPair(), randomBytes(NONCE_SIZE));
        }

        ClientHandshake(byte[] psk, KeyPair keyPair, byte[] nonce) {
            if (psk == null || psk.length != 32) throw new IllegalArgumentException("invalid screen key");
            this.psk = psk.clone();
            this.keyPair = keyPair;
            hello = concat(encodePoint((ECPublicKey) keyPair.getPublic()), nonce);
        }

        /** Verifies the server hello and derives the session keys and the client finish MAC. */
        SessionKeys finish(byte[] serverHello) throws IOException {
            if (serverHello == null || serverHello.length != SERVER_HELLO_SIZE) {
                throw new IOException("invalid live screen server hello");
            }
            byte[] pubNonce = Arrays.copyOfRange(serverHello, 0, POINT_SIZE + NONCE_SIZE);
            byte[] transcript = transcript(hello, pubNonce);
            byte[] expected = handshakeMac(psk, "server finished\u0000", transcript);
            if (!MessageDigest.isEqual(expected, Arrays.copyOfRange(serverHello, POINT_SIZE + NONCE_SIZE, SERVER_HELLO_SIZE))) {
                throw new PasswordMismatchException();
            }
            ECPublicKey serverKey = decodePoint(Arrays.copyOfRange(serverHello, 0, POINT_SIZE),
                    ((ECPublicKey) keyPair.getPublic()).getParams());
            byte[] shared;
            try {
                KeyAgreement agreement = KeyAgreement.getInstance("ECDH");
                agreement.init(keyPair.getPrivate());
                agreement.doPhase(serverKey, true);
                shared = leftPad(agreement.generateSecret(), 32);
            } catch (Exception ex) {
                throw new IOException("live screen key agreement failed", ex);
            }
            byte[] prk = ScreenKeys.hkdfExtract(transcript, concat(shared, psk));
            byte[] c2s = ScreenKeys.hkdfExpand(prk, "DeskFerry c2s".getBytes(StandardCharsets.UTF_8), 32);
            byte[] s2c = ScreenKeys.hkdfExpand(prk, "DeskFerry s2c".getBytes(StandardCharsets.UTF_8), 32);
            return new SessionKeys(c2s, s2c, handshakeMac(psk, "client finished\u0000", transcript));
        }
    }

    static byte[] transcript(byte[] clientHello, byte[] serverPubNonce) {
        try {
            MessageDigest digest = MessageDigest.getInstance("SHA-256");
            digest.update("DeskFerry live v2\u0000".getBytes(StandardCharsets.UTF_8));
            digest.update(clientHello);
            digest.update(serverPubNonce);
            return digest.digest();
        } catch (Exception ex) {
            throw new IllegalStateException("SHA-256 is unavailable", ex);
        }
    }

    static byte[] handshakeMac(byte[] psk, String label, byte[] transcript) {
        return ScreenKeys.hmacSha256(psk, label.getBytes(StandardCharsets.UTF_8), transcript);
    }

    // ---- Secure records -----------------------------------------------------------------

    static byte[] recordNonce(long seq) {
        byte[] nonce = new byte[12];
        for (int i = 0; i < 8; i++) nonce[4 + i] = (byte) (seq >>> (56 - 8 * i));
        return nonce;
    }

    /** One direction of AES-256-GCM records with its own sequence counter. Not thread safe. */
    static final class RecordCipher {
        private final SecretKeySpec key;
        private final Cipher cipher;
        private long seq;

        RecordCipher(byte[] key) {
            this.key = new SecretKeySpec(key, "AES");
            try {
                cipher = Cipher.getInstance("AES/GCM/NoPadding");
            } catch (Exception ex) {
                throw new IllegalStateException("AES-GCM is unavailable", ex);
            }
        }

        long sequence() {
            return seq;
        }

        /** Returns length(4) || AES-GCM(type || payload) and advances the sequence. */
        byte[] seal(byte type, byte[] payload) throws IOException {
            int payloadLength = payload == null ? 0 : payload.length;
            if (payloadLength + 1 > MAX_RECORD_PLAIN) throw new IOException("live screen record is too large");
            byte[] plain = new byte[payloadLength + 1];
            plain[0] = type;
            if (payloadLength > 0) System.arraycopy(payload, 0, plain, 1, payloadLength);
            try {
                cipher.init(Cipher.ENCRYPT_MODE, key, new GCMParameterSpec(GCM_TAG_BYTES * 8, recordNonce(seq)));
                byte[] sealed = cipher.doFinal(plain);
                byte[] out = new byte[4 + sealed.length];
                putInt(out, 0, sealed.length);
                System.arraycopy(sealed, 0, out, 4, sealed.length);
                seq++;
                return out;
            } catch (Exception ex) {
                throw new IOException("live screen record encryption failed", ex);
            }
        }

        /** Decrypts one record body (without its length prefix); returns type || payload. */
        byte[] open(byte[] sealed) throws IOException {
            if (sealed.length < GCM_TAG_BYTES + 1 || sealed.length > MAX_RECORD_CIPHER) {
                throw new IOException("invalid live screen record size " + sealed.length);
            }
            try {
                cipher.init(Cipher.DECRYPT_MODE, key, new GCMParameterSpec(GCM_TAG_BYTES * 8, recordNonce(seq)));
                byte[] plain = cipher.doFinal(sealed);
                seq++;
                return plain;
            } catch (Exception ex) {
                throw new IOException("live screen record failed authentication");
            }
        }

        /** Reads and decrypts the next record from {@code in}; returns type || payload. */
        byte[] read(DataInputStream in) throws IOException {
            int size = in.readInt();
            if (size < GCM_TAG_BYTES + 1 || size > MAX_RECORD_CIPHER) {
                throw new IOException("invalid live screen record size " + Integer.toUnsignedString(size));
            }
            byte[] sealed = new byte[size];
            in.readFully(sealed);
            return open(sealed);
        }
    }

    // ---- Work-to-client records ---------------------------------------------------------

    static final class FrameStart {
        final long seq;
        final int width;
        final int height;
        final int flags;

        FrameStart(long seq, int width, int height, int flags) {
            this.seq = seq;
            this.width = width;
            this.height = height;
            this.flags = flags;
        }

        boolean keyframe() {
            return (flags & FRAME_FLAG_KEY) != 0;
        }
    }

    static FrameStart parseFrameStart(byte[] payload, int offset, int length) throws IOException {
        if (length != 17) throw new IOException("invalid frame start record");
        long seq = getLong(payload, offset);
        long width = getUInt(payload, offset + 8);
        long height = getUInt(payload, offset + 12);
        if (width <= 0 || height <= 0 || width > 16384 || height > 16384) throw new IOException("invalid frame size");
        return new FrameStart(seq, (int) width, (int) height, payload[offset + 16] & 0xFF);
    }

    static final class FrameEnd {
        final long seq;
        final int flags;

        FrameEnd(long seq, int flags) {
            this.seq = seq;
            this.flags = flags;
        }

        boolean lossless() {
            return (flags & FRAME_END_LOSSLESS) != 0;
        }

        boolean settled() {
            return (flags & FRAME_END_SETTLED) != 0;
        }
    }

    static FrameEnd parseFrameEnd(byte[] payload, int offset, int length) throws IOException {
        if (length != 9) throw new IOException("invalid frame end record");
        return new FrameEnd(getLong(payload, offset), payload[offset + 8] & 0xFF);
    }

    static final class Rect {
        final int x;
        final int y;
        final int w;
        final int h;
        final int enc;
        final byte[] data;
        final int dataOffset;
        final int dataLength;

        Rect(int x, int y, int w, int h, int enc, byte[] data, int dataOffset, int dataLength) {
            this.x = x;
            this.y = y;
            this.w = w;
            this.h = h;
            this.enc = enc;
            this.data = data;
            this.dataOffset = dataOffset;
            this.dataLength = dataLength;
        }

        /** For an H.264 rect: whether this access unit starts a new stream. */
        boolean videoStreamStart() {
            return enc == ENC_H264 && dataLength > 0 && (data[dataOffset] & VIDEO_STREAM_START) != 0;
        }

        /** For an H.264 rect: offset and length of the Annex B access unit after the flags byte. */
        int videoOffset() {
            return dataOffset + 1;
        }

        int videoLength() {
            return Math.max(0, dataLength - 1);
        }

        /** Solid fill as an Android ARGB colour (opaque). */
        int solidArgb() {
            return 0xFF000000 | (data[dataOffset] & 0xFF) << 16 | (data[dataOffset + 1] & 0xFF) << 8 | (data[dataOffset + 2] & 0xFF);
        }
    }

    /** Parses a rects record; rect data slices reference {@code payload}. */
    static List<Rect> parseRects(byte[] payload, int offset, int length) throws IOException {
        if (length < 2) throw new IOException("invalid rects record");
        int end = offset + length;
        int count = getUShort(payload, offset);
        int pos = offset + 2;
        ArrayList<Rect> rects = new ArrayList<>(count);
        for (int i = 0; i < count; i++) {
            if (end - pos < RECT_HEADER_SIZE) throw new IOException("truncated rects record");
            int x = getUShort(payload, pos);
            int y = getUShort(payload, pos + 2);
            int w = getUShort(payload, pos + 4);
            int h = getUShort(payload, pos + 6);
            int enc = payload[pos + 8] & 0xFF;
            long size = getUInt(payload, pos + 9);
            pos += RECT_HEADER_SIZE;
            if (size > end - pos || w <= 0 || h <= 0) throw new IOException("invalid rect in rects record");
            if (enc == ENC_SOLID && size != 4) throw new IOException("invalid solid rect");
            if (enc != ENC_SOLID && enc != ENC_PNG && enc != ENC_JPEG && enc != ENC_JPEG_SCALED && enc != ENC_H264) throw new IOException("unsupported rect encoding " + enc);
            rects.add(new Rect(x, y, w, h, enc, payload, pos, (int) size));
            pos += (int) size;
        }
        return rects;
    }

    static final class Cursor {
        final int x;
        final int y;
        final boolean visible;
        final long shapeId;
        final int hotX;
        final int hotY;
        final byte[] png;

        Cursor(int x, int y, boolean visible, long shapeId, int hotX, int hotY, byte[] png) {
            this.x = x;
            this.y = y;
            this.visible = visible;
            this.shapeId = shapeId;
            this.hotX = hotX;
            this.hotY = hotY;
            this.png = png;
        }
    }

    static Cursor parseCursor(byte[] payload, int offset, int length) throws IOException {
        if (length < 21) throw new IOException("invalid cursor record");
        int x = getInt(payload, offset);
        int y = getInt(payload, offset + 4);
        boolean visible = payload[offset + 8] != 0;
        long shape = getUInt(payload, offset + 9);
        int hotX = getUShort(payload, offset + 13);
        int hotY = getUShort(payload, offset + 15);
        long size = getUInt(payload, offset + 17);
        if (size != length - 21) throw new IOException("invalid cursor shape length");
        byte[] png = Arrays.copyOfRange(payload, offset + 21, offset + length);
        return new Cursor(x, y, visible, shape, hotX, hotY, png);
    }

    static byte[] encodeSeq(long seq) {
        byte[] out = new byte[8];
        putLong(out, 0, seq);
        return out;
    }

    // ---- View mapping -------------------------------------------------------------------

    /**
     * Maps a view point to framebuffer pixels through a scale + translate image matrix, clamped to
     * the framebuffer. Returns {x, y}.
     */
    static int[] viewToFramebuffer(float x, float y, float scale, float translateX, float translateY, int width, int height) {
        float fx = (x - translateX) / (scale <= 0 ? 1f : scale);
        float fy = (y - translateY) / (scale <= 0 ? 1f : scale);
        int ix = (int) Math.floor(fx);
        int iy = (int) Math.floor(fy);
        return new int[]{Math.max(0, Math.min(width - 1, ix)), Math.max(0, Math.min(height - 1, iy))};
    }

    // ---- Byte helpers -------------------------------------------------------------------

    static byte[] randomBytes(int size) {
        byte[] out = new byte[size];
        new SecureRandom().nextBytes(out);
        return out;
    }

    static byte[] concat(byte[] a, byte[] b) {
        byte[] out = Arrays.copyOf(a, a.length + b.length);
        System.arraycopy(b, 0, out, a.length, b.length);
        return out;
    }

    private static byte[] leftPad(byte[] value, int size) {
        if (value.length == size) return value;
        if (value.length > size) return Arrays.copyOfRange(value, value.length - size, value.length);
        byte[] out = new byte[size];
        System.arraycopy(value, 0, out, size - value.length, value.length);
        return out;
    }

    static int getUShort(byte[] b, int off) {
        return (b[off] & 0xFF) << 8 | (b[off + 1] & 0xFF);
    }

    static int getInt(byte[] b, int off) {
        return (b[off] & 0xFF) << 24 | (b[off + 1] & 0xFF) << 16 | (b[off + 2] & 0xFF) << 8 | (b[off + 3] & 0xFF);
    }

    static long getUInt(byte[] b, int off) {
        return getInt(b, off) & 0xFFFFFFFFL;
    }

    static long getLong(byte[] b, int off) {
        return (getUInt(b, off) << 32) | getUInt(b, off + 4);
    }

    static void putInt(byte[] b, int off, int value) {
        b[off] = (byte) (value >>> 24);
        b[off + 1] = (byte) (value >>> 16);
        b[off + 2] = (byte) (value >>> 8);
        b[off + 3] = (byte) value;
    }

    static void putLong(byte[] b, int off, long value) {
        putInt(b, off, (int) (value >>> 32));
        putInt(b, off + 4, (int) value);
    }
}
