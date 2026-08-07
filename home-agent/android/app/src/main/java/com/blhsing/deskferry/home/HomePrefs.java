package com.blhsing.deskferry.home;

import android.content.Context;
import android.content.SharedPreferences;

import org.json.JSONArray;
import org.json.JSONException;
import org.json.JSONObject;

import java.util.ArrayList;
import java.util.List;

final class HomePrefs {
    static final String PREFS = "deskferry_home";
    static final String PREF_RELAY_URL = "relay_url";
    static final String PREF_LOCAL_PORT = "local_port";
	static final String PREF_LOCAL_SMB_PORT = "local_smb_port";
    static final String PREF_PROXY = "proxy";
    static final String PREF_LOG_RETENTION_DAYS = "log_retention_days";
    static final String PREF_DESTINATIONS = "destinations";
    static final String PREF_SELECTED_DESTINATION = "selected_destination";
    static final int DEFAULT_LOCAL_PORT = 3389;
	static final int DEFAULT_LOCAL_SMB_PORT = 1445;
    static final int DEFAULT_LOG_RETENTION_DAYS = 7;

    private HomePrefs() {
    }

    static String loadRelayUrl(Context context) {
        return prefs(context).getString(PREF_RELAY_URL, RelayUrls.DEFAULT_RELAY_URL);
    }

    static int loadLocalPort(Context context) {
        return sanitizePort(prefs(context).getInt(PREF_LOCAL_PORT, DEFAULT_LOCAL_PORT));
    }

	static int loadLocalSMBPort(Context context) {
		return sanitizeSMBPort(prefs(context).getInt(PREF_LOCAL_SMB_PORT, DEFAULT_LOCAL_SMB_PORT));
	}

    static String loadProxy(Context context) {
        return prefs(context).getString(PREF_PROXY, ProxySettings.DEFAULT);
    }

    static int loadLogRetentionDays(Context context) {
        return sanitizeLogRetentionDays(prefs(context).getInt(PREF_LOG_RETENTION_DAYS, DEFAULT_LOG_RETENTION_DAYS));
    }

    static void save(Context context, String relayUrl, int port, String proxy) {
        prefs(context)
                .edit()
                .putString(PREF_RELAY_URL, relayUrl)
                .putInt(PREF_LOCAL_PORT, sanitizePort(port))
                .putString(PREF_PROXY, proxy)
                .apply();
    }

    static final class Destination {
        String name;
        String relayBases;
        String room;
        String roomProof;

        Destination(String name, String relayUrl) {
            this(name, relayUrl, "");
        }

        Destination(String name, String relayUrl, String roomProof) {
            this.name = sanitizeName(name);
            String legacy = relayUrl == null ? "" : relayUrl.trim();
            this.room = RelayUrls.roomFromRelayUrls(legacy);
            try {
                this.relayBases = RelayUrls.joinRelayUrls(RelayUrls.normalizeRelayBaseUrls(legacy));
            } catch (Exception ignored) {
                this.relayBases = RelayUrls.joinRelayUrls(RelayUrls.DEFAULT_RELAY_BASE_URLS);
            }
            this.roomProof = roomProof == null ? "" : roomProof.trim();
        }

        Destination(String name, String relayBases, String room, String roomProof) {
            this.name = sanitizeName(name);
            this.relayBases = relayBases == null ? "" : relayBases.trim();
            this.room = room == null || room.trim().isEmpty() ? RelayUrls.DEFAULT_ROOM : room.trim();
            this.roomProof = roomProof == null ? "" : roomProof.trim();
        }

        String relayUrl() {
            try {
                return RelayUrls.joinRelayUrls(RelayUrls.relayRoomUrls(RelayUrls.normalizeRelayBaseUrls(relayBases), room));
            } catch (Exception ignored) {
                return RelayUrls.DEFAULT_RELAY_URL;
            }
        }
    }

    static List<Destination> loadDestinations(Context context) {
        ArrayList<Destination> result = new ArrayList<>();
        String encoded = prefs(context).getString(PREF_DESTINATIONS, "");
        if (encoded != null && !encoded.trim().isEmpty()) {
            try {
                JSONArray array = new JSONArray(encoded);
                for (int i = 0; i < array.length(); i++) {
                    JSONObject item = array.getJSONObject(i);
                    String name = sanitizeName(item.optString("name", "Work"));
                    String relayBases = item.optString("relay_bases", "").trim();
                    String room = item.optString("room", "").trim();
                    String relayUrl = item.optString("relay_url", "").trim();
                    if (!relayBases.isEmpty()) {
                        result.add(new Destination(uniqueName(result, name), relayBases, room,
                                item.optString("room_proof", "")));
                    } else if (!relayUrl.isEmpty()) {
                        result.add(new Destination(uniqueName(result, name), relayUrl, item.optString("room_proof", "")));
                    }
                }
            } catch (JSONException ignored) {
                result.clear();
            }
        }
        if (result.isEmpty()) {
            SharedPreferences preferences = prefs(context);
            if (preferences.contains(PREF_RELAY_URL)) {
                result.add(new Destination("Work", loadRelayUrl(context)));
            } else {
                result.add(new Destination("Work", RelayUrls.joinRelayUrls(RelayUrls.DEFAULT_RELAY_BASE_URLS),
                        RelayUrls.DEFAULT_ROOM, ""));
            }
        }
        return result;
    }

    static int loadSelectedDestination(Context context, int count) {
        int selected = prefs(context).getInt(PREF_SELECTED_DESTINATION, 0);
        return selected >= 0 && selected < count ? selected : 0;
    }

    static void saveDestinations(Context context, List<Destination> destinations, int selected, int port) {
        saveDestinations(context, destinations, selected, port, loadProxy(context));
    }

    static void saveDestinations(Context context, List<Destination> destinations, int selected, int port, String proxy) {
        saveDestinations(context, destinations, selected, port, proxy, loadLogRetentionDays(context));
    }

    static void saveDestinations(Context context, List<Destination> destinations, int selected, int port, String proxy, int logRetentionDays) {
		saveDestinations(context, destinations, selected, port, proxy, logRetentionDays, loadLocalSMBPort(context));
	}

	static void saveDestinations(Context context, List<Destination> destinations, int selected, int port, String proxy, int logRetentionDays, int smbPort) {
        JSONArray array = new JSONArray();
        for (Destination destination : destinations) {
            JSONObject item = new JSONObject();
            try {
                item.put("name", sanitizeName(destination.name));
                item.put("relay_bases", destination.relayBases == null ? "" : destination.relayBases.trim());
                item.put("room", destination.room == null ? RelayUrls.DEFAULT_ROOM : destination.room.trim());
                item.put("room_proof", destination.roomProof == null ? "" : destination.roomProof.trim());
            } catch (JSONException ignored) {
            }
            array.put(item);
        }
        String selectedRelay = destinations.isEmpty()
                ? RelayUrls.DEFAULT_RELAY_URL
                : destinations.get(Math.max(0, Math.min(selected, destinations.size() - 1))).relayUrl();
        prefs(context).edit()
                .putString(PREF_DESTINATIONS, array.toString())
                .putInt(PREF_SELECTED_DESTINATION, Math.max(0, selected))
                .putString(PREF_RELAY_URL, selectedRelay)
                .putInt(PREF_LOCAL_PORT, sanitizePort(port))
				.putInt(PREF_LOCAL_SMB_PORT, sanitizeSMBPort(smbPort))
                .putString(PREF_PROXY, proxy)
                .putInt(PREF_LOG_RETENTION_DAYS, sanitizeLogRetentionDays(logRetentionDays))
                .apply();
    }

    static String loadSelectedRoomProof(Context context) {
        List<Destination> destinations = loadDestinations(context);
        if (destinations.isEmpty()) {
            return "";
        }
        return destinations.get(loadSelectedDestination(context, destinations.size())).roomProof;
    }

    private static String sanitizeName(String value) {
        String name = value == null ? "" : value.trim();
        return name.isEmpty() ? "Work" : name;
    }

    private static String uniqueName(List<Destination> destinations, String requested) {
        String base = sanitizeName(requested);
        String candidate = base;
        int suffix = 2;
        while (containsName(destinations, candidate)) {
            candidate = base + " " + suffix++;
        }
        return candidate;
    }

    private static boolean containsName(List<Destination> destinations, String name) {
        for (Destination destination : destinations) {
            if (destination.name.equalsIgnoreCase(name)) {
                return true;
            }
        }
        return false;
    }

    static int sanitizePort(int port) {
        return port > 0 && port < 65536 ? port : DEFAULT_LOCAL_PORT;
    }

	static int sanitizeSMBPort(int port) {
		return port >= 1024 && port < 65536 ? port : DEFAULT_LOCAL_SMB_PORT;
	}

    static int sanitizeLogRetentionDays(int days) {
        return days >= 1 && days <= 3650 ? days : DEFAULT_LOG_RETENTION_DAYS;
    }

    private static SharedPreferences prefs(Context context) {
        return context.getSharedPreferences(PREFS, Context.MODE_PRIVATE);
    }
}
