package main

import "testing"

func TestValidateReadOnlyToolCall(t *testing.T) {
	tests := []struct {
		name    string
		tool    string
		args    map[string]interface{}
		wantErr bool
	}{
		{
			name: "select allowed",
			tool: "query_1c",
			args: map[string]interface{}{
				"query": "  SELECT * FROM Справочник.Номенклатура  ",
			},
			wantErr: false,
		},
		{
			name: "vybrat allowed",
			tool: "query_1c",
			args: map[string]interface{}{
				"query": "  ВЫБРАТЬ * ИЗ Документ.Заказ  ",
			},
			wantErr: false,
		},
		{
			name: "update blocked",
			tool: "query_1c",
			args: map[string]interface{}{
				"query": "UPDATE Справочник.Номенклатура SET Наименование = 'x'",
			},
			wantErr: true,
		},
		{
			name: "delete blocked",
			tool: "execute_sql",
			args: map[string]interface{}{
				"sql": "DELETE FROM РегистрСведений.Цены",
			},
			wantErr: true,
		},
		{
			name: "non sql tool ignored",
			tool: "get_weather",
			args: map[string]interface{}{
				"city": "Moscow",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateReadOnlyToolCall(tt.tool, tt.args)
			if tt.wantErr && err == nil {
				t.Errorf("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}
