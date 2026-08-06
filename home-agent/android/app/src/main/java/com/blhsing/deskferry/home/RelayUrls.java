package com.blhsing.deskferry.home;

import android.util.Base64;

import java.net.URI;
import java.net.URISyntaxException;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import java.util.Locale;

final class RelayUrls {
    static final String DEFAULT_RELAY_URL = "https://test-officialwebsite.azurewebsites.net/relay/workdesk";
    static final String DEFAULT_ROOM = "workdesk";
    static final List<String> DEFAULT_RELAY_BASE_URLS = Collections.unmodifiableList(java.util.Arrays.asList(
            "https://test-officialwebsite.azurewebsites.net/relay",
            "http://217.142.228.117/relay"));

    private RelayUrls() {
    }

    static String normalizeRelayUrl(String value) throws URISyntaxException {
        String raw = value == null ? "" : value.trim();
        if (raw.isEmpty()) {
            raw = DEFAULT_RELAY_URL;
        }
        URI uri = new URI(raw);
        String scheme = lower(uri.getScheme());
        if ("wss".equals(scheme)) {
            scheme = "https";
        } else if ("ws".equals(scheme)) {
            scheme = "http";
        } else if (!"https".equals(scheme) && !"http".equals(scheme)) {
            throw new URISyntaxException(raw, "relay URL must start with https:// or http://");
        }
        if (uri.getHost() == null || uri.getHost().isEmpty()) {
            throw new URISyntaxException(raw, "relay URL must include a host");
        }
        String path = stripTrailingSlash(emptyAs(uri.getRawPath(), "/relay"));
        if (path.isEmpty()) {
            path = "/relay";
        }
        if (path.endsWith("/ws")) {
            path = path.substring(0, path.length() - 3);
            if (path.isEmpty()) {
                path = "/relay";
            }
        }
        return new URI(scheme, uri.getUserInfo(), uri.getHost(), uri.getPort(), path, uri.getRawQuery(), null).toString();
    }

    static String normalizeRelayBaseUrl(String value) throws URISyntaxException {
        URI uri = new URI(normalizeRelayUrl(value));
        String path = stripTrailingSlash(emptyAs(uri.getRawPath(), "/relay"));
        String[] parts = path.replaceAll("^/+|/+$", "").split("/");
        if (parts.length >= 2 && "relay".equals(parts[0]) && !isReservedRelayPart(parts[1])) {
            path = "/relay";
        }
        return new URI(uri.getScheme(), uri.getUserInfo(), uri.getHost(), uri.getPort(), path, null, null).toString();
    }

    static List<String> normalizeRelayBaseUrls(String value) throws URISyntaxException {
        List<String> raw = splitRelayUrls(value);
        if (raw.isEmpty()) raw = DEFAULT_RELAY_BASE_URLS;
        ArrayList<String> out = new ArrayList<>();
        for (String item : raw) {
            String normalized = normalizeRelayBaseUrl(item);
            boolean seen = false;
            for (String existing : out) if (existing.equalsIgnoreCase(normalized)) { seen = true; break; }
            if (!seen) out.add(normalized);
        }
        return Collections.unmodifiableList(out);
    }

    static List<String> relayRoomUrls(List<String> bases, String room) throws URISyntaxException {
        room = room == null ? "" : room.trim();
        if (room.isEmpty() || room.matches(".*[/\\\\?#].*")) throw new URISyntaxException(room, "room name is required and must not contain URL separators");
        ArrayList<String> out = new ArrayList<>();
        for (String base : bases) out.add(normalizeRelayBaseUrl(base) + "/" + room);
        return Collections.unmodifiableList(out);
    }

    static String roomFromRelayUrls(String value) {
        for (String item : splitRelayUrls(value)) {
            String room = roomToken(item, "");
            if (!"default".equals(room)) return room;
        }
        return DEFAULT_ROOM;
    }

    static List<String> normalizeRelayUrls(String value) throws URISyntaxException {
        return normalizeRelayUrls(value, true);
    }

    static List<String> normalizeRelayUrls(String value, boolean useDefault) throws URISyntaxException {
        List<String> raw = splitRelayUrls(value);
        if (raw.isEmpty() && useDefault) {
            raw = Collections.singletonList(DEFAULT_RELAY_URL);
        }
        ArrayList<String> out = new ArrayList<>();
        for (String relayUrl : raw) {
            String normalized = normalizeRelayUrl(relayUrl);
            boolean seen = false;
            for (String existing : out) {
                if (existing.equalsIgnoreCase(normalized)) {
                    seen = true;
                    break;
                }
            }
            if (!seen) {
                out.add(normalized);
            }
        }
        return Collections.unmodifiableList(out);
    }

    static String joinRelayUrls(List<String> relayUrls) {
        if (relayUrls == null || relayUrls.isEmpty()) {
            return DEFAULT_RELAY_URL;
        }
        StringBuilder builder = new StringBuilder();
        for (String relayUrl : relayUrls) {
            if (builder.length() > 0) {
                builder.append('\n');
            }
            builder.append(relayUrl);
        }
        return builder.toString();
    }

    static String primaryRelayUrl(String relayUrls) {
        try {
            List<String> normalized = normalizeRelayUrls(relayUrls);
            return normalized.isEmpty() ? DEFAULT_RELAY_URL : normalized.get(0);
        } catch (URISyntaxException ex) {
            return DEFAULT_RELAY_URL;
        }
    }

    static String webSocketEndpoint(String relayUrl) throws URISyntaxException {
        URI uri = new URI(normalizeRelayUrl(relayUrl));
        String scheme = "https".equals(lower(uri.getScheme())) ? "wss" : "ws";
        String path = stripTrailingSlash(emptyAs(uri.getRawPath(), "/relay"));
        if (path.isEmpty() || "/".equals(path)) {
            path = "/relay/ws";
        } else if (!path.endsWith("/ws") && !path.endsWith("/dashboard")) {
            path = path + "/ws";
        }
        return new URI(scheme, uri.getUserInfo(), uri.getHost(), uri.getPort(), path, uri.getRawQuery(), null).toString();
    }

    static String dashboardUrl(String relayUrl) {
        try {
            return normalizeRelayUrl(primaryRelayUrl(relayUrl));
        } catch (URISyntaxException ex) {
            return DEFAULT_RELAY_URL;
        }
    }

    static String roomToken(String relayUrl, String configuredToken) {
        String token = configuredToken == null ? "" : configuredToken.trim();
        if (!token.isEmpty()) {
            return token;
        }
        try {
            URI uri = new URI(relayUrl == null ? "" : relayUrl.trim());
            String path = uri.getPath();
            if (path != null) {
                String[] parts = path.replaceAll("^/+|/+$", "").split("/");
                if (parts.length >= 2 && "relay".equals(parts[0])) {
                    String room = parts[1];
                    if (!room.isEmpty()
                            && !"ws".equals(room)
                            && !"status".equals(room)
                            && !"health".equals(room)
                            && !"dashboard".equals(room)) {
                        return room;
                    }
                }
            }
            String queryToken = queryValue(uri.getRawQuery(), "room");
            if (!queryToken.isEmpty()) {
                return queryToken;
            }
            queryToken = queryValue(uri.getRawQuery(), "token");
            if (!queryToken.isEmpty()) {
                return queryToken;
            }
        } catch (URISyntaxException ignored) {
        }
        return "default";
    }

    static String roomPasswordProof(String relayUrl, String password) {
        String value = password == null ? "" : password;
        if (value.isEmpty()) {
            return "";
        }
        String room = roomToken(relayUrl, "").trim().toLowerCase(Locale.ROOT);
        String material = "DeskFerry room credential v1\u0000" + room + "\u0000" + value;
        try {
            byte[] digest = MessageDigest.getInstance("SHA-256")
                    .digest(material.getBytes(StandardCharsets.UTF_8));
            return Base64.encodeToString(digest, Base64.URL_SAFE | Base64.NO_PADDING | Base64.NO_WRAP);
        } catch (Exception ex) {
            throw new IllegalStateException("SHA-256 is unavailable", ex);
        }
    }

    static String rdpAddress(int port) {
        return "127.0.0.1:" + port;
    }

    private static String queryValue(String rawQuery, String key) {
        if (rawQuery == null || rawQuery.isEmpty()) {
            return "";
        }
        for (String pair : rawQuery.split("&")) {
            int sep = pair.indexOf('=');
            String name = sep >= 0 ? pair.substring(0, sep) : pair;
            if (key.equals(name)) {
                return sep >= 0 ? pair.substring(sep + 1).trim() : "";
            }
        }
        return "";
    }

    private static List<String> splitRelayUrls(String value) {
        String raw = value == null ? "" : value.trim();
        if (raw.isEmpty()) {
            return Collections.emptyList();
        }
        String[] parts = raw.split("[\\r\\n;,]+");
        ArrayList<String> out = new ArrayList<>(parts.length);
        for (String part : parts) {
            String relayUrl = part.trim();
            if (!relayUrl.isEmpty()) {
                out.add(relayUrl);
            }
        }
        return out;
    }

    private static String emptyAs(String value, String fallback) {
        return value == null || value.isEmpty() ? fallback : value;
    }

    private static String stripTrailingSlash(String value) {
        String out = value == null ? "" : value;
        while (out.length() > 1 && out.endsWith("/")) {
            out = out.substring(0, out.length() - 1);
        }
        return out;
    }

    private static String lower(String value) {
        return value == null ? "" : value.toLowerCase(Locale.ROOT);
    }

    private static boolean isReservedRelayPart(String value) {
        return "ws".equals(value) || "status".equals(value) || "health".equals(value) || "dashboard".equals(value);
    }
}
